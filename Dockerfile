FROM cgr.dev/chainguard/static:latest
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/nyala /usr/local/bin/nyala
ENTRYPOINT ["nyala"]
