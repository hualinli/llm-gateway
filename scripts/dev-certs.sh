#!/bin/sh
# dev-certs.sh — 本地可重现的 Control 平面 mTLS 证书与主密钥 bootstrap。
#
# 生成（默认 data/dev-certs/，已被 .gitignore 忽略）：
#   ca.crt / ca.key                       本地 CA
#   control.crt / control.key             Control gRPC 服务端证书（SAN: control, localhost, 127.0.0.1）
#   gateway-client.crt / gateway-client.key   Gateway 的 mTLS 客户端证书
#   console-client.crt / console-client.key   Console 的 mTLS 客户端证书（CN=console，与
#                                             CONTROL_CONSOLE_SERVICE_NAME 对应）
#   provider-credential-master-key        base64 编码的 32 字节集群主密钥（格式与 control.EnsureMasterKeyFile 一致）
#
# 用法：
#   scripts/dev-certs.sh [--force] [dir]
#
# 幂等：文件齐全且未指定 --force 时只打印导出变量并退出 0，绝不覆盖；
# 只存在部分文件时拒绝继续（避免留下半套证书）。--force 会整体重新生成。
# 脚本只打印路径和 CA 指纹，不打印任何私钥或主密钥内容。
#
# 环境变量名与 internal/control/tls.go 的 control.TLSConfigFromEnv() 完全一致。

set -eu

FORCE=0
DIR=""

usage() {
	cat <<'EOF'
usage: scripts/dev-certs.sh [--force] [dir]

  --force   重新生成全部文件（会覆盖 dir 下已存在的同名文件）
  dir       输出目录，默认 $DEV_CERTS_DIR 或 data/dev-certs

生成的导出变量与 internal/control/tls.go 读取的名字一致：
  CONTROL_TLS_CA_FILE CONTROL_TLS_CERT_FILE CONTROL_TLS_KEY_FILE
  CONTROL_TLS_CLIENT_CERT_FILE CONTROL_TLS_CLIENT_KEY_FILE
  CONTROL_TLS_SERVER_NAME CONTROL_TLS_REQUIRE_CLIENT_CERT
  CONTROL_CONSOLE_SERVICE_NAME
  PROVIDER_CREDENTIAL_MASTER_KEY_FILE
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--force | -f)
		FORCE=1
		;;
	--help | -h)
		usage
		exit 0
		;;
	--*)
		echo "dev-certs: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$DIR" ]; then
			echo "dev-certs: unexpected extra argument: $1" >&2
			usage >&2
			exit 2
		fi
		DIR="$1"
		;;
	esac
	shift
done

if [ -z "$DIR" ]; then
	DIR="${DEV_CERTS_DIR:-data/dev-certs}"
fi

if ! command -v openssl >/dev/null 2>&1; then
	echo "dev-certs: openssl is required but was not found in PATH" >&2
	exit 1
fi

# CORE 是「整套证书」的判定集合；CONSOLE 是 T03 新增的 Console 客户端证书。
# 旧目录（只有 CORE）不会被判为损坏：脚本会只补生成 Console 客户端证书。
CORE_FILES="ca.crt ca.key control.crt control.key gateway-client.crt gateway-client.key provider-credential-master-key"
CONSOLE_FILES="console-client.crt console-client.key"
FILES="$CORE_FILES $CONSOLE_FILES"

all_present=1
any_present=0
for file in $CORE_FILES; do
	if [ -e "$DIR/$file" ]; then
		any_present=1
	else
		all_present=0
	fi
done

console_present=1
console_any=0
for file in $CONSOLE_FILES; do
	if [ -e "$DIR/$file" ]; then
		console_any=1
	else
		console_present=0
	fi
done

print_exports() {
	abs=$(cd "$DIR" && pwd)
	echo ""
	echo "本地 Control mTLS 材料目录: ${abs}（只有路径，没有私钥内容）"
	if [ -f "$DIR/ca.crt" ]; then
		fingerprint=$(openssl x509 -in "$DIR/ca.crt" -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//')
		[ -n "$fingerprint" ] && echo "CA SHA-256 指纹: $fingerprint"
	fi
	echo ""
	echo "export CONTROL_TLS_CA_FILE=$abs/ca.crt"
	echo "export CONTROL_TLS_CERT_FILE=$abs/control.crt"
	echo "export CONTROL_TLS_KEY_FILE=$abs/control.key"
	echo "export CONTROL_TLS_REQUIRE_CLIENT_CERT=true"
	echo "export CONTROL_TLS_CLIENT_CERT_FILE=$abs/gateway-client.crt"
	echo "export CONTROL_TLS_CLIENT_KEY_FILE=$abs/gateway-client.key"
	echo "export CONTROL_TLS_SERVER_NAME=control"
	echo "export CONTROL_CONSOLE_SERVICE_NAME=console"
	echo "# Console -> Control 管理通道（internal/controlclient 读取，客户端证书 CN=console）:"
	echo "export CONTROL_ADDR=localhost:50051"
	echo "export CONTROL_TLS_CLIENT_CERT_FILE=$abs/console-client.crt   # Console 覆盖上面 Gateway 的客户端证书"
	echo "export CONTROL_TLS_CLIENT_KEY_FILE=$abs/console-client.key"
	echo "export CONSOLE_PUBLICATION_INTERVAL=15s"
	echo "export PROVIDER_CREDENTIAL_MASTER_KEY_FILE=$abs/provider-credential-master-key"
	echo ""
	echo "生产 profile 还会校验 PostgreSQL/管理认证/诊断端口等，见 docs/decisions/ADR-0001-runtime-profiles.md。"
}

if [ "$FORCE" -eq 0 ]; then
	if [ "$all_present" -eq 1 ] && [ "$console_present" -eq 1 ]; then
		echo "dev-certs: 已存在完整的一套文件，未做任何修改（需要重新生成请加 --force）"
		print_exports
		exit 0
	fi
	if [ "$any_present" -eq 1 ] && [ "$all_present" -ne 1 ]; then
		echo "dev-certs: $DIR 中存在不完整的证书文件，拒绝覆盖；请先清理该目录或使用 --force" >&2
		exit 1
	fi
	if [ "$console_any" -eq 1 ] && [ "$console_present" -ne 1 ]; then
		echo "dev-certs: $DIR 中 Console 客户端证书不完整，拒绝覆盖；请先清理或使用 --force" >&2
		exit 1
	fi
fi

umask 077
mkdir -p "$DIR"

# 旧目录升级路径：CA 与其它证书都在，只缺 T03 的 Console 客户端证书时，用现有 CA
# 只补生成这两个文件，不重新生成（也就不会让已分发的 Gateway 证书失效）。
if [ "$FORCE" -eq 0 ] && [ "$all_present" -eq 1 ] && [ "$console_present" -ne 1 ]; then
	if ! command -v openssl >/dev/null 2>&1; then
		echo "dev-certs: openssl is required but was not found in PATH" >&2
		exit 1
	fi
	STAGE="$DIR/.stage.$$"
	rm -rf "$STAGE"
	mkdir -p "$STAGE"
	trap 'rm -rf "$STAGE"' EXIT INT TERM
	CONF="$STAGE/openssl.cnf"
	cat >"$CONF" <<'EOF'
[ req ]
distinguished_name = req_dn
prompt = no

[ req_dn ]
CN = llm-gateway-local

[ client_ext ]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid,issuer
EOF
	echo "dev-certs: 现有证书齐全，只补生成 Console 客户端证书到 $DIR ..."
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STAGE/console-client.key" 2>/dev/null
	openssl req -new -key "$STAGE/console-client.key" -out "$STAGE/console-client.csr" -subj "/CN=console" -config "$CONF"
	openssl x509 -req -in "$STAGE/console-client.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" \
		-CAserial "$STAGE/ca.srl" -CAcreateserial \
		-sha256 -days 825 -extfile "$CONF" -extensions client_ext -out "$STAGE/console-client.crt" 2>/dev/null
	chmod 600 "$STAGE/console-client.key"
	chmod 644 "$STAGE/console-client.crt"
	mv -f "$STAGE/console-client.key" "$DIR/console-client.key"
	mv -f "$STAGE/console-client.crt" "$DIR/console-client.crt"
	echo "dev-certs: 完成（私钥 0600，证书 0644）"
	print_exports
	exit 0
fi

STAGE="$DIR/.stage.$$"
rm -rf "$STAGE"
mkdir -p "$STAGE"
trap 'rm -rf "$STAGE"' EXIT INT TERM

CONF="$STAGE/openssl.cnf"
cat >"$CONF" <<'EOF'
[ req ]
distinguished_name = req_dn
prompt = no

[ req_dn ]
CN = llm-gateway-local

[ ca_ext ]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash

[ server_ext ]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @server_alt
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid,issuer

[ client_ext ]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid,issuer

[ server_alt ]
DNS.1 = control
DNS.2 = localhost
IP.1 = 127.0.0.1
EOF

echo "dev-certs: 生成 CA 与证书到 $DIR ..."
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STAGE/ca.key" 2>/dev/null
openssl req -new -key "$STAGE/ca.key" -out "$STAGE/ca.csr" -subj "/CN=llm-gateway-local-ca" -config "$CONF"
openssl x509 -req -in "$STAGE/ca.csr" -signkey "$STAGE/ca.key" -sha256 -days 3650 \
	-extfile "$CONF" -extensions ca_ext -out "$STAGE/ca.crt" 2>/dev/null

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STAGE/control.key" 2>/dev/null
openssl req -new -key "$STAGE/control.key" -out "$STAGE/control.csr" -subj "/CN=control" -config "$CONF"
openssl x509 -req -in "$STAGE/control.csr" -CA "$STAGE/ca.crt" -CAkey "$STAGE/ca.key" -CAcreateserial \
	-sha256 -days 825 -extfile "$CONF" -extensions server_ext -out "$STAGE/control.crt" 2>/dev/null

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STAGE/gateway-client.key" 2>/dev/null
openssl req -new -key "$STAGE/gateway-client.key" -out "$STAGE/gateway-client.csr" -subj "/CN=gateway-client" -config "$CONF"
openssl x509 -req -in "$STAGE/gateway-client.csr" -CA "$STAGE/ca.crt" -CAkey "$STAGE/ca.key" -CAcreateserial \
	-sha256 -days 825 -extfile "$CONF" -extensions client_ext -out "$STAGE/gateway-client.crt" 2>/dev/null

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$STAGE/console-client.key" 2>/dev/null
openssl req -new -key "$STAGE/console-client.key" -out "$STAGE/console-client.csr" -subj "/CN=console" -config "$CONF"
openssl x509 -req -in "$STAGE/console-client.csr" -CA "$STAGE/ca.crt" -CAkey "$STAGE/ca.key" -CAcreateserial \
	-sha256 -days 825 -extfile "$CONF" -extensions client_ext -out "$STAGE/console-client.crt" 2>/dev/null

openssl rand -base64 32 | tr -d '\n' >"$STAGE/provider-credential-master-key"

for file in $FILES; do
	chmod 600 "$STAGE/$file"
done
chmod 644 "$STAGE/ca.crt" "$STAGE/control.crt" "$STAGE/gateway-client.crt" "$STAGE/console-client.crt"

for file in $FILES; do
	mv -f "$STAGE/$file" "$DIR/$file"
done

chmod 700 "$DIR"
for file in $FILES; do
	case "$file" in
	*.crt) chmod 644 "$DIR/$file" ;;
	*) chmod 600 "$DIR/$file" ;;
	esac
done

echo "dev-certs: 完成（私钥 0600，证书 0644，主密钥 0600）"
print_exports
