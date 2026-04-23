# syntax=docker/dockerfile:1.7

FROM node:20-alpine AS build
WORKDIR /app
COPY package.json ./
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine AS runtime
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY entrypoint.sh /docker-entrypoint.d/00-landing-tls.sh
RUN chmod +x /docker-entrypoint.d/00-landing-tls.sh
COPY --from=build /app/.output/public /usr/share/nginx/html/site
EXPOSE 80 443
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget --no-check-certificate -qO- https://127.0.0.1/site/ >/dev/null 2>&1 || exit 1
