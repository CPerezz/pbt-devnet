# besu-pbt with the offline-migration hook: `besu` becomes images/besu/besu,
# which applies a pending swap before handing over to besu's own start script.
ARG BASE=besu-pbt:base
FROM ${BASE}
USER root
RUN mv /opt/besu/bin/besu /opt/besu/bin/besu.real
COPY --chmod=755 besu /opt/besu/bin/besu
