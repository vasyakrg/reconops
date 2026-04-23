#!/bin/sh
# Self-signed cert on first start for local dev. In prod bind-mount real
# certs to /etc/nginx/certs/server.{crt,key} and this is a no-op.
set -e

CERT=/etc/nginx/certs/server.crt
KEY=/etc/nginx/certs/server.key
CN=${RECON_LANDING_TLS_CN:-reconops.ru}

if [ ! -f "$CERT" ] || [ ! -f "$KEY" ]; then
  echo "landing-entrypoint: generating self-signed cert for CN=$CN"
  mkdir -p /etc/nginx/certs
  apk add --no-cache openssl >/dev/null
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$KEY" -out "$CERT" -days 825 \
    -subj "/CN=$CN" \
    -addext "subjectAltName=DNS:$CN,DNS:localhost,IP:127.0.0.1"
fi

exec nginx -g 'daemon off;'
