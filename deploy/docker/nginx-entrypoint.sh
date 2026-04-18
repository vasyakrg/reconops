#!/bin/sh
# Generate a self-signed cert on first start so local dev / smoke testing
# works out of the box. For production: bind-mount your real certs at
# /etc/nginx/certs/server.{crt,key} and this is a no-op.
set -e

CERT=/etc/nginx/certs/server.crt
KEY=/etc/nginx/certs/server.key
CN=${RECON_TLS_CN:-localhost}

if [ ! -f "$CERT" ] || [ ! -f "$KEY" ]; then
  echo "nginx-entrypoint: generating self-signed cert for CN=$CN"
  mkdir -p /etc/nginx/certs
  apk add --no-cache openssl >/dev/null
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$KEY" -out "$CERT" -days 825 \
    -subj "/CN=$CN" \
    -addext "subjectAltName=DNS:$CN,DNS:localhost,IP:127.0.0.1"
fi

exec nginx -g 'daemon off;'
