import os
import sys
import json
import yaml
import ldap3
import base64
import urllib
import hashlib
import logging
import argparse

class LdapClient:
    def __init__(self, url, binddn, passwd):

        logger.debug("Creating ldap3 Server and Connection")
        self.server = ldap3.Server(url, get_info=ldap3.ALL)
        self.conn = ldap3.Connection(self.server, binddn, passwd, auto_bind=True)

    # result will be a tuple of boolean success and the entries
    def search(self, base, scope, what):
        # returns a boolean indicating result, but even if failed, the entries will be the empty list, so we ignore the return value
        _ = self.conn.search(base, what, scope, attributes=ldap3.ALL_ATTRIBUTES)
        # attributes=['*', '+']

        # entries is a list of entry
        # you can do funny things with entry:

        # print(entry.entry_to_ldif())
        # print(entry.entry_to_json())
        # print(entry.entry_dn)

        return self.conn.entries

    def add(self, dn, objectClass, attributes):
        # dn is a string, objectClass a list (or scalar), attributes a dict
        return self.conn.add(dn, objectClass, attributes)

    # modify: try to implement something which behaves like "original" ldapmodify, even if it is awkward?

    # This is how a json should look like
    #
    # {
    #     "dn": "olcDatabase={1}mdb,cn=config",
    #     "changetype": "modify",
    #     "replace": "olcAccess",
    #     "attributes": {
    #         "olcAccess": [
    #             "{0}to dn.subtree=\"ou=Mail,dc=example,dc=org\" attrs=userPassword by self write by dn.children=\"ou=Readpw,dc=example,dc=org\" read by anonymous auth by * none",
    #             "{1}to attrs=userPassword by self write by anonymous auth by * none",
    #             "{2}to attrs=shadowLastChange by self write by * read",
    #             "{3}to * by * read"
    #         ]
    #     }
    # }

    # and it should translate into a call like this:
    #
    # dn='olcDatabase={1}mdb,cn=config'
    # conn.modify(dn, {
    #     'sn': [
    #         (ldap3.MODIFY_ADD, ['Young', 'Johnson']),
    #         (ldap3.MODIFY_DELETE, ['Smith'])
    #     ],
    #     'givenname': [(ldap3.MODIFY_REPLACE, ['Mary', 'Jane'])]
    # })

    def modify(self, dn, ops):
        return self.conn.modify(dn, ops)
    
    def get_result(self):
        return self.conn.result

def handle(ldapClient, sj):
    if sj['changetype']=="add":
        success = ldapClient.add(sj['dn'], sj['objectClass'], sj['attributes'])
        result = ldapClient.get_result()
        info = f"additional info: result=\"{result['result']}\" description=\"{result['description']}\" message=\"{result['message']}\""
        if success:
            logger.info(f"Result: success ({info})")
        else:
            logger.warning(f"Result: failed ({info})")
    elif sj['changetype']=="modify":
        if 'replace' in sj:
            success = ldapClient.modify(sj['dn'], { sj['replace']: [(ldap3.MODIFY_REPLACE, sj['attributes'][sj['replace']])]})
            result = ldapClient.get_result()
            info = f"additional info: result=\"{result['result']}\" description=\"{result['description']}\" message=\"{result['message']}\""
            if success:
                logger.info(f"Result: success ({info})")
            else:
                logger.warning(f"Result: failed ({info})")
        else:
            logger.error("changetype not yet implemented, please contribute")

def main():
    global logger

    description="""
Generic ldap client using the pure-python ldap3 library with some
additional convenience ontop.
"""

    epilog="""
Basic usage: Use -H -D -w as known from standard ldap-utils and provide a
json carrying the usual ldapadd or ldapmodify information. (Since ldap3
does not support parsing LDIFs, we chose to use json with a similar
schema.) The actual json schema docuemntation is missing, please refer to
the samples and / or the source of this script for reference.

Convenience feature: Supplying a domain is not required for basic
operation, but for some of the other "convenience" features below. The
domain can be supplied either as DNS-style domain (e.g. chuck-chuck-chuck.net)
or in "dc=...,dc=..." syntax (e.g. "dc=chuck-chuck-chuck,dc=net") and is
converted automatically if required.

Convenience feature: If -D is omitted, defaults to "cn=admin,<domain-dc>"
if a domain was provided.

Convenience feature: supplying -w can be replaced by specifying a
values.slapd.secret.yaml file containing the required password.
Heuristically determines: if the binddn ends with "cn=config", it picks the
'root' password from the yaml file, otherwise the 'admin' password.

Convenience feature: can populate "readpw" users based on a
ldap-readpw-users.secret.yml file contents. In that case the "--domain"
argument gets required as well.

Bugs: I consider it a design bug that this script is mixing generic
ldapclient functionality with project-specific "smartness". Should be
separated.

auto-readpw.yml:
    <user1>: <password1>
    <user2>: <password2>
    [...]

values.slapd.secret.yaml:
    .secret.root.plain for the cn=admin,cn=config DN
    .secret.admin.plain otherwise

environment variables:
    LDAP_URI       <-> -H
    LDAP_BINDDN    <-> -D
    LDAP_BINDPW    <-> -w
    DOMAIN_DC      <-> --domain
"""

    parser = argparse.ArgumentParser(formatter_class=argparse.RawDescriptionHelpFormatter, description=description, epilog=epilog)
    parser.add_argument("-v", "--values", help="Read values from yaml file", type=str)
    parser.add_argument("-d", "--debug", help="debug output", action='store_true')
    parser.add_argument("-H", "--ldapuri", help="ldap uri", type=str)
    #parser.add_argument("-b", "--base", help="ldap base", type=str)
    parser.add_argument("-D", "--binddn", help="ldap binddn", type=str)
    parser.add_argument("-w", "--bindpw", help="ldap bindpw", type=str)
    parser.add_argument("-C", "--chdir", help="Change directory first", type=str)
    parser.add_argument("-s", "--secrets", help="Read values.slapd.secret.yaml file", type=str)
    #parser.add_argument("-r", "--root-pw", help="Root password", type=str)
    #parser.add_argument("-a", "--admin-pw", help="Admin password", type=str)
    parser.add_argument("--auto-readpw", help="Automatically add readpw users from a yaml file", type=str)
    parser.add_argument("--readpw-ou", help="OU name for read-only service accounts", type=str, default="Readpw")
    parser.add_argument("--domain", help="LDAP domain", type=str)
    parser.add_argument("jsons", help="Pseudo-LDIF JSON files", type=str, nargs=argparse.REMAINDER)

    args = parser.parse_args()

    logging.basicConfig(stream=sys.stdout)
    logger = logging.getLogger("ldap-bootstrap.py")
    if args.debug:
        logger.setLevel(logging.DEBUG)
    else:
        logger.setLevel(logging.INFO)

    node={}
    if args.values:
        try:
            with open(args.values, 'r') as stream:
                node = yaml.safe_load(stream)
        except Exception as e:
            logger.error(e)
            logger.error("Error reading values file!")
            sys.exit(1)

    auto_readpw={}
    if args.auto_readpw:
        try:
            with open(args.auto_readpw, 'r') as stream:
                auto_readpw = yaml.safe_load(stream) or {}
        except Exception as e:
            logger.error(e)
            logger.error("Error reading auto-readpw file!")
            sys.exit(1)

    secrets={}
    if args.secrets:
        try:
            with open(args.secrets, 'r') as stream:
                secrets = yaml.safe_load(stream)
                secrets = secrets.get('secret', {})
        except Exception as e:
            logger.error(e)
            logger.error("Error reading secrets file!")
            sys.exit(1)

    # So, as usual: precedence:
    # 1) explicit arguments
    # 2) values from yson files
    # 3) environment?

    ldapuri = args.ldapuri or node.get('ldap_endpoint', None) or os.environ.get("LDAP_URI", None)
    domain = args.domain or os.environ.get("DOMAIN_DC", None)
    if domain:
        if not domain.startswith("dc="):
            domain = ",".join([ f"dc={dc}" for dc in domain.split(".") ])

    binddn = args.binddn or os.environ.get('LDAP_BINDDN', None)
    if not binddn:
        if domain:
            binddn=f"cn=admin,{domain}"

    bindpw = args.bindpw or secrets.get('root' if binddn.endswith('cn=config') else 'admin', {}).get('plain', None) or os.environ.get('LDAP_BINDPW', None)

    if args.auto_readpw and not domain:
        print("With --auto-readpw, --domain is required")
        parser.print_help()
        sys.exit(1)

    if not ldapuri or not binddn or not bindpw:
        if not ldapuri:
            print("ldapuri is None!")
        if not binddn:
            print("binddn is None!")
        if not bindpw:
            print("bindpw is None!")
        parser.print_help()
        sys.exit(1)

    # so I ported this script from using "ldap" (the non-pure-python implementation)
    # and ldap3 is a bit more picky in the syntax of the url, and I don't want to
    # change the interface of the script, so let's implement a lenient parsing here
    # goal: remove the trailing slash if exits
    # the _replace() method looks like a funny internal, but it is documented
    # https://docs.python.org/3.9/library/urllib.parse.html
    ldapuri = urllib.parse.urlparse(ldapuri)._replace(path="").geturl()

    ldapClient = LdapClient(ldapuri, binddn, bindpw)

    additional_jsons = []
    if args.auto_readpw:
        additional_jsons += [
            {
                'dn': f"uid={u},ou={args.readpw_ou},{domain}",
                'changetype': 'add',
                'objectClass': ['account', 'posixAccount'],
                'attributes': {
                    'cn': u,
                    'uid': u,
                    'uidnumber': '65534',
                    'gidnumber': '65534',
                    'homeDirectory': '/dev/null',
                    'userPassword': auto_readpw[u]
                }
            } for u in auto_readpw.keys()
        ]

    for s in args.jsons:
        try:
            with open(s, 'r') as f:
                sj = json.load(f)

                entries = []
                if isinstance(sj, dict):
                    logger.debug("json seems to be a dict")
                    entries = [ sj ]
                elif isinstance(sj, list):
                    logger.debug("json seems to be a list")
                    entries = sj
                else:
                    logger.error("Should not happen")
                    raise RuntimeError

                for i in entries:
                    logger.info(f"Applying content from {f.name} for {i['dn']}")
                    handle(ldapClient, i)
        except Exception as e:
            logger.error(e)
            logger.error("Error reading ldap content json file!")
            logger.error("Skipping and trying potential next one...")
            raise
    if additional_jsons:
        for i in additional_jsons:
            logger.info(f"Adding automatic readpw user from {args.auto_readpw} for {i['dn']}")
            handle(ldapClient, i)

if __name__ == "__main__":
    main()
