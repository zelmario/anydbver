#!/bin/bash
# Configure, initialize and unseal a single-node HashiCorp Vault for anydbver.
# Plain HTTP on :8200 and file storage, as in KB0057067 (PG_TDE integration with
# Hashicorp Vault). A lab setup: one unseal key, kept on this node.
#
# Afterwards:
#   - kv-v2 secrets engine at secret/
#   - policy "pg-tde" that can manage keys under secret/ only
#   - token with the fixed id below and that policy, which pg-tde nodes read
#     from here over HTTP instead of having the token copied to them
#   - root token in /root/.vault-token, so "vault" works for root right away
set -e

# Keep in sync with tools/setup_pg_tde.sh
PG_TDE_TOKEN=anydbver-pg-tde-token

export VAULT_ADDR=http://127.0.0.1:8200

mkdir -p /etc/vault.d /var/lib/vault

cat > /etc/vault.d/vault.hcl << EOF
ui = true
listener "tcp" {
  address     = "0.0.0.0:8200"
  tls_disable = 1
}
storage "file" {
  path = "/var/lib/vault"
}
api_addr = "http://$(node_ip.sh):8200"
disable_mlock = true
EOF

id vault &>/dev/null || useradd --system --home /etc/vault.d --shell /bin/false vault
chown -R vault:vault /etc/vault.d /var/lib/vault

# A container restart leaves Vault sealed, and every encrypted table on the
# pg-tde nodes unreadable until it is unsealed. Unseal on start with the stored
# key, a lab convenience that has no place in production.
cat > /usr/local/bin/vault-unseal.sh << 'EOF'
#!/bin/bash
export VAULT_ADDR=http://127.0.0.1:8200
[ -f /root/vault-init.txt ] || exit 0
until curl -s -o /dev/null $VAULT_ADDR/v1/sys/health ; do sleep 1; done
/usr/local/bin/vault operator unseal "$(awk '/^Unseal Key 1:/{print $4}' /root/vault-init.txt)" > /dev/null
EOF
chmod 0700 /usr/local/bin/vault-unseal.sh

cat > /etc/systemd/system/vault.service << EOF
[Unit]
Description=HashiCorp Vault - A tool for managing secrets
Documentation=https://developer.hashicorp.com/vault/docs
Requires=network-online.target
After=network-online.target
ConditionFileNotEmpty=/etc/vault.d/vault.hcl

[Service]
User=vault
Group=vault
ExecStart=/usr/local/bin/vault server -config=/etc/vault.d/vault.hcl
ExecStartPost=+/usr/local/bin/vault-unseal.sh
ExecReload=/bin/kill --signal HUP \$MAINPID
KillMode=process
KillSignal=SIGINT
Restart=on-failure
RestartSec=5
TimeoutStopSec=30
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/profile.d/vault.sh << EOF
export VAULT_ADDR=$VAULT_ADDR
EOF

systemctl daemon-reload
systemctl enable vault
systemctl start vault

# /sys/health answers 501 until initialized and 503 while sealed, curl -s
# without -f only waits for the listener.
until curl -s -o /dev/null $VAULT_ADDR/v1/sys/health ; do sleep 1; done

if [ ! -f /root/vault-init.txt ] ; then
  vault operator init -key-shares=1 -key-threshold=1 > /root/vault-init.txt
  chmod 0600 /root/vault-init.txt
fi
vault operator unseal "$(awk '/^Unseal Key 1:/{print $4}' /root/vault-init.txt)" > /dev/null
awk '/^Initial Root Token:/{print $4}' /root/vault-init.txt > /root/.vault-token
chmod 0600 /root/.vault-token
export VAULT_TOKEN=$(cat /root/.vault-token)

# Wait for the node to become active after unsealing.
until vault status > /dev/null 2>&1 ; do sleep 1; done

vault secrets list | grep -q '^secret/' || vault secrets enable -path=secret kv-v2

# Least privilege for pg_tde: its keys under secret/, plus reading the mount
# description, which pg_tde checks to confirm the engine is kv version 2.
vault policy write pg-tde - << 'EOF'
path "secret/data/*" {
  capabilities = ["create", "read", "update", "list"]
}
path "secret/metadata/*" {
  capabilities = ["read", "list"]
}
path "sys/mounts/secret" {
  capabilities = ["read"]
}
EOF

vault token lookup "$PG_TDE_TOKEN" > /dev/null 2>&1 ||
  vault token create -id="$PG_TDE_TOKEN" -policy=pg-tde -orphan -period=768h \
    -display-name=anydbver-pg-tde > /dev/null

touch /root/vault.configured
