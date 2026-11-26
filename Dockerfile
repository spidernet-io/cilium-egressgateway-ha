FROM alpine:3
ARG TARGETARCH
# controller or agent
ARG COMPONENT
COPY bin/linux_${TARGETARCH}/${COMPONENT} /usr/local/bin/${COMPONENT}
