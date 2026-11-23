FROM alpine:3
ARG TARGETARCH=amd64
COPY bin/linux_${TARGETARCH}/ /usr/local/bin/
