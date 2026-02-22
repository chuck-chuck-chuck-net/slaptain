#!/bin/bash

# https://stackoverflow.com/questions/1527049/how-can-i-join-elements-of-a-bash-array-into-a-delimited-string
join_by() {
    local d=${1-} f=${2-}
    if shift 2; then
        printf %s "$f" "${@/#/$d}"
    fi
}

sts=""
service=""
dns_names=""
namespace=""

usage() { echo "Usage: $0 <secret>" 1>&2; exit 1; }
while getopts ":d:n:s:t:" o; do
    case "${o}" in
        d)
            dns_names="$OPTARG"
            ;;
        n)
            namespace="$OPTARG"
            ;;
        s)
            service="$OPTARG"
            ;;
        t)
            sts="$OPTARG"
            ;;
        *)
            usage
            ;;
    esac
done
shift $((OPTIND-1))

if [[ $# -lt 1 ]]
then
    usage
fi

secret="$1"
if [[ ${secret%-tls}-tls == ${secret} ]]
then
    label=${secret%-tls}
else
    echo "Error: secret must follow the convention to end in -tls; got: $secret" 1>&2
    usage
fi

if [[ -z "$namespace" ]]
then
    namespace=$(</var/run/secrets/kubernetes.io/serviceaccount/namespace)
fi

# echo "Testing if secret $secret in namespace $namespace already exists:"
# if kubectl get secret -n $namespace $secret
# then
#     echo "... yes -- stopping here, not re-creating."
#     exit 0
# else
#     echo "... nope -- continuing, going creating it."
# fi

# I was unable to find reference docs for how the kubelet-serving approver works
# but experimentally it is to be observed that some special /O.../CN subj is required
# otherwise the cert will not get approved, but failed

domain=$(sed -ne '/^search/s/.* svc.\([^ ]*\).*/\1/pg' /etc/resolv.conf)

subj="/O=system:nodes/CN=system:node:$label.$namespace.svc.$domain"
# no sans just based on the label
sans=()

if [[ -n "$dns_names" ]]
then
    for d in $dns_names
    do
        sans+=( "DNS:$d" )
    done
fi

if [[ -n "$service" ]]
then
    subj="/O=system:nodes/CN=system:node:$service.$namespace.svc.$domain"
    sans+=(
        "DNS:$service"
        "DNS:$service.$namespace.svc.$domain"
        "DNS:$service.$namespace"
        "DNS:$service.$namespace.svc"
    )
fi

if [[ -n "$sts" ]]
then
    subj="/O=system:nodes/CN=system:node:$sts-*.$sts.$namespace.svc.$domain"
    sans+=("DNS:$sts-*.$sts.$namespace.svc.$domain")
fi

joined_sans=$(join_by ", " "${sans[@]}")

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -out $label.key

openssl req -new -subj "$subj" -addext "subjectAltName = $joined_sans" -key $label.key -out $label.csr

cat >$label-$namespace-csr.yaml <<EOF
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: $label-$namespace-csr
spec:
  request: $(base64 -w 0 <$label.csr)
  signerName: kubernetes.io/kubelet-serving
  usages:
  - digital signature
  - key encipherment
  - server auth
EOF

# experimentally, patching the csr does no good
# we will end up with updated key, and old csr, and thus
# a mismatch between private and public key
kubectl delete csr $label-$namespace-csr --ignore-not-found=true
kubectl apply -f $label-$namespace-csr.yaml
kubectl certificate approve $label-$namespace-csr

# because we put the "kubernetes.io/kubelet-serving" signer name there, it will be automatically issued by the k8s root ca

# wait for the csr to go to condition "Issued"
# which seems technically to be the same as .status.certificate exists
# cf minio-operator, csr.go, lines 181ff
kubectl wait certificatesigningrequest $label-$namespace-csr --for='jsonpath={.status.certificate}' --timeout=600s

# nomenclature: no namespace in the secret name because it's namespaced anyways
# the certificatesigningrequest name carries the namespace name because it is a cluster resource
cat >$secret.yaml <<EOF
apiVersion: v1
kind: Secret
type: kubernetes.io/tls
metadata:
  name: $secret
data:
  ca.crt: $(kubectl get cm -n $namespace kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' | base64 -w 0)
  tls.crt: $(kubectl get certificatesigningrequest $label-$namespace-csr -o jsonpath='{.status.certificate}')
  tls.key: $(base64 -w 0 <$label.key)
EOF

kubectl apply -n $namespace -f $secret.yaml
