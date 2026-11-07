# Built by goreleaser (dockers_v2): the build context holds the prebuilt
# binaries under <os>/<arch>/, selected by TARGETPLATFORM.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/gocached /usr/local/bin/gocached
ENV GOCACHED_ADDR=:8080 \
    GOCACHED_CACHE_DIR=/cache
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gocached"]
CMD ["serve"]
