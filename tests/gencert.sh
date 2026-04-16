#!/bin/bash

# https://stackoverflow.com/questions/1527049/how-can-i-join-elements-of-a-bash-array-into-a-delimited-string
join_by() {
    local d=${1-} f=${2-}
    if shift 2; then
        printf %s "$f" "${@/#/$d}"
    fi
}

# kubectl wrapper that injects --context when -c was given.
kube_context=""
kctl() {
    if [[ -n "$kube_context" ]]; then
        kubectl --context "$kube_context" "$@"
    else
        kubectl "$@"
    fi
}

sts=""
service=""
headless_svc=""
dns_names=""
ip_sans=""
namespace=""

usage() { echo "Usage: $0 [-c context] [-d dns_names] [-H headless_svc] [-i ips] [-n namespace] [-s service] [-t statefulset] <secret>" 1>&2; exit 1; }
while getopts ":c:d:H:i:n:s:t:" o; do
    case "${o}" in
        c)
            kube_context="$OPTARG"
            ;;
        d)
            dns_names="$OPTARG"
            ;;
        H)
            headless_svc="$OPTARG"
            ;;
        i)
            ip_sans="$OPTARG"
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

# If the TLS secret already exists there is nothing to do.
if kctl get secret -n "$namespace" "$secret" &>/dev/null; then
    echo "Secret $secret already exists in namespace $namespace — skipping."
    exit 0
fi

# I was unable to find reference docs for how the kubelet-serving approver works
# but experimentally it is to be observed that some special /O.../CN subj is required
# otherwise the cert will not get approved, but failed

# Stage 1: running inside a pod — /etc/resolv.conf has the cluster search path.
domain=$(sed -ne '/^search/s/.* svc\.\([^ ]*\).*/\1/pg' /etc/resolv.conf)

# Stage 2: running from a developer laptop with kubectl — query coredns Corefile.
if [[ -z "$domain" ]]; then
    domain=$(kctl get configmap coredns -n kube-system \
        -o jsonpath='{.data.Corefile}' 2>/dev/null \
        | awk '/kubernetes/{print $2; exit}')
fi

# Stage 3: kube-dns clusters store it in a dedicated key.
if [[ -z "$domain" ]]; then
    domain=$(kctl get configmap kube-dns -n kube-system \
        -o jsonpath='{.data.domain}' 2>/dev/null || true)
fi

# Final fallback — nearly every cluster uses cluster.local.
if [[ -z "$domain" ]]; then
    echo "Warning: could not auto-discover cluster domain; defaulting to cluster.local" >&2
    domain="cluster.local"
fi

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
    # RFC 6125 / OpenSSL 3: * must be the entire leftmost label (no partial wildcards).
    sans+=("DNS:*.$sts.$namespace.svc.$domain")
fi

if [[ -n "$headless_svc" ]]
then
    sans+=(
        "DNS:$headless_svc"
        "DNS:$headless_svc.$namespace"
        "DNS:$headless_svc.$namespace.svc"
        "DNS:$headless_svc.$namespace.svc.$domain"
        # RFC 6125 / OpenSSL 3 compliant wildcard: * is the entire leftmost label.
        # Covers all per-pod DNS names: slapd-N.<headless>.<ns>.svc.<domain>
        "DNS:*.$headless_svc.$namespace.svc.$domain"
    )
fi

# IP SANs — for NodePort access from outside the cluster.
if [[ -n "$ip_sans" ]]; then
    IFS=',' read -ra ips <<< "$ip_sans"
    for ip in "${ips[@]}"; do
        sans+=("IP:$ip")
    done
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

# Clean up any stale CSR from a previous failed run before creating a fresh one.
kctl delete csr $label-$namespace-csr --ignore-not-found=true
kctl apply -f $label-$namespace-csr.yaml
kctl certificate approve $label-$namespace-csr

# because we put the "kubernetes.io/kubelet-serving" signer name there, it will be automatically issued by the k8s root ca

# wait for the csr to go to condition "Issued"
# which seems technically to be the same as .status.certificate exists
# cf minio-operator, csr.go, lines 181ff
kctl wait certificatesigningrequest $label-$namespace-csr --for='jsonpath={.status.certificate}' --timeout=600s

# nomenclature: no namespace in the secret name because it's namespaced anyways
# the certificatesigningrequest name carries the namespace name because it is a cluster resource
cat >$secret.yaml <<EOF
apiVersion: v1
kind: Secret
type: kubernetes.io/tls
metadata:
  name: $secret
data:
  ca.crt: $(kctl get cm -n $namespace kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' | base64 -w 0)
  tls.crt: $(kctl get certificatesigningrequest $label-$namespace-csr -o jsonpath='{.status.certificate}')
  tls.key: $(base64 -w 0 <$label.key)
EOF

kctl apply -n $namespace -f $secret.yaml
