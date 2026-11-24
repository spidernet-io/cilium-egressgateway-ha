FROM alpine:3
ARG TARGETARCH=amd64
# controller or agent
ARG COMPONENT
COPY bin/linux_${TARGETARCH}/${COMPONENT} /usr/local/bin/${COMPONENT}
