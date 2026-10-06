# nethermind-pbt with the offline-migration hook in front of the image's own
# entrypoint (WORKDIR /nethermind is inherited).
ARG BASE=nethermind-pbt:base
FROM ${BASE}
COPY --chmod=755 pbt-swap /usr/local/bin/pbt-swap
ENTRYPOINT ["/usr/local/bin/pbt-swap"]
