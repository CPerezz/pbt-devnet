# pbt-geth with the offline-migration hook: `geth` becomes images/geth/geth,
# which runs a pending import before handing over to the real binary.
ARG BASE=pbt-geth:base
FROM ${BASE}
RUN mv /usr/local/bin/geth /usr/local/bin/geth.real
COPY --chmod=755 geth /usr/local/bin/geth
