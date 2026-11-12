#!/bin/bash
# pg_tde with a HashiCorp Vault global key provider, following KB0057067
# (PG_TDE integration with Hashicorp Vault).
#
#   setup_pg_tde.sh token VAULT_IP KEY_NAME
#       Wait for the Vault deployed by the "vault" keyword, store the pg_tde
#       token in /etc/pg_tde/vault.token and the settings in
#       /etc/pg_tde/pg_tde.env, and install this script as
#       /usr/local/bin/setup_pg_tde.sh for Patroni's post_init hook.
#   setup_pg_tde.sh preload POSTGRESQL_CONF
#       Add pg_tde to shared_preload_libraries in that file, keeping whatever
#       is already there. Run before the server starts.
#   setup_pg_tde.sh keys
#       On a running primary: create the extension, the Vault key provider,
#       the principal key, and make it the default key for every database.
#       Connects as postgres with the usual PG* variables.
set -e

# psql and pg_isready live outside PATH on EL.
export PATH=$PATH:$(ls -d /usr/pgsql-*/bin /usr/lib/postgresql/*/bin 2>/dev/null | tr '\n' ':')

ENV_FILE=/etc/pg_tde/pg_tde.env
TOKEN_FILE=/etc/pg_tde/vault.token
# Created by tools/setup_vault_server.sh on the Vault node.
PG_TDE_TOKEN=anydbver-pg-tde-token
PROVIDER=anydbver-vault
# The kv-v2 mount the Vault node enables, as in the KB.
MOUNT=secret

case "$1" in
token)
  VAULT_IP="$2"
  KEY_NAME="$3"
  VAULT_URL="http://${VAULT_IP}:8200"
  if [ -z "$VAULT_IP" ] ; then
    echo "pg-tde needs a Vault server, deploy one and point at it with vault=nodeN" >&2
    exit 1
  fi

  # The Vault node is deployed in parallel with this one, so wait for it to
  # be initialized, unsealed and to have created the pg_tde token.
  for i in $(seq 1 300) ; do
    if curl -sf -o /dev/null -H "X-Vault-Token: $PG_TDE_TOKEN" "$VAULT_URL/v1/auth/token/lookup-self" ; then
      READY=1
      break
    fi
    sleep 2
  done
  if [ -z "$READY" ] ; then
    echo "Vault at $VAULT_URL is not ready, or has no pg_tde token after 10 minutes" >&2
    exit 1
  fi

  mkdir -p /etc/pg_tde
  echo -n "$PG_TDE_TOKEN" > "$TOKEN_FILE"
  cat > "$ENV_FILE" << EOF
VAULT_URL=$VAULT_URL
KEY_NAME=$KEY_NAME
EOF
  chown -R postgres:postgres /etc/pg_tde
  chmod 0700 /etc/pg_tde
  chmod 0600 "$TOKEN_FILE" "$ENV_FILE"

  install -m 0755 "$0" /usr/local/bin/setup_pg_tde.sh
  ;;

preload)
  CONF="$2"
  # Last setting wins in postgresql.conf, so read the effective value, add
  # pg_tde in front and append the result.
  CURRENT=$(grep -E "^\s*shared_preload_libraries\s*=" "$CONF" | tail -n 1 |
    sed -E "s/^[^=]*=\s*'?([^'#]*)'?.*$/\1/; s/\s//g")
  case ",$CURRENT," in
    *,pg_tde,*) exit 0 ;;
  esac
  NEW="pg_tde${CURRENT:+,$CURRENT}"
  echo "shared_preload_libraries = '$NEW'" >> "$CONF"
  ;;

keys)
  source "$ENV_FILE"
  PSQL="psql -v ON_ERROR_STOP=1 -U postgres -X -q"
  until pg_isready -q ; do sleep 1; done
  until $PSQL -d postgres -c "SELECT 1" > /dev/null ; do sleep 1; done

  # template1 too, so databases created later have the extension and use the
  # default key without any extra step.
  for DB in template1 postgres ; do
    $PSQL -d $DB -c "CREATE EXTENSION IF NOT EXISTS pg_tde"
  done

  $PSQL -d postgres << EOF
SELECT pg_tde_add_global_key_provider_vault_v2('$PROVIDER', '$VAULT_URL', '$MOUNT', '$TOKEN_FILE', NULL)
  WHERE NOT EXISTS (SELECT 1 FROM pg_tde_list_all_global_key_providers() WHERE name = '$PROVIDER');
SELECT pg_tde_create_key_using_global_key_provider('$KEY_NAME', '$PROVIDER');
SELECT pg_tde_set_default_key_using_global_key_provider('$KEY_NAME', '$PROVIDER');
EOF
  ;;

*)
  echo "usage: $0 token VAULT_IP KEY_NAME | preload POSTGRESQL_CONF | keys" >&2
  exit 1
  ;;
esac
