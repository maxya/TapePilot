FROM node:24.22.1-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS build
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build
FROM nginx:1.31.3-alpine@sha256:4a73073bd557c65b759505da037898b61f1be6cbcc3c2c3aeac22d2a470c1752
COPY deploy/nginx.conf /etc/nginx/nginx.conf
COPY --from=build /web/build/ /srv/web/
USER 101:101
ENTRYPOINT ["nginx","-g","daemon off;"]
