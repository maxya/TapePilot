FROM node:24.22.1-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS build
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
COPY .node-version /.node-version
ARG SOURCE_DATE_EPOCH=0
RUN SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" npm run build
FROM nginx:1.31.3-alpine-slim@sha256:45b82ed5f285b90d63df07ba70430fdd8f25624b416617d9e6dc93412b2006dc
RUN apk add --no-cache --upgrade \
    libcrypto3=3.5.9-r1 libssl3=3.5.9-r1 \
    musl=1.2.6-r3 musl-utils=1.2.6-r3 \
    pcre2=10.49-r0 zlib=1.4-r0
COPY deploy/nginx.conf /etc/nginx/nginx.conf
COPY --from=build /web/build/ /srv/web/
USER 101:101
ENTRYPOINT ["nginx","-g","daemon off;"]
