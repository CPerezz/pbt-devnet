# Overlay on the built geth image: the real binary moves aside and the
# EIP-8347 migration shim takes its place at the exact name ethereum-package's
# launcher invokes — `geth`, inside one `sh -c` string covering both init and
# the server run, which is why a Docker ENTRYPOINT could not do this job.
#
# FROM the staged binary tag, never pbt-geth:local itself: a self-referencing
# base would move the previous shim to geth.real on the next rebuild and the
# shim would exec itself forever.
FROM pbt-geth-binary:local
RUN mv /usr/local/bin/geth /usr/local/bin/geth.real
COPY geth-shim.sh /usr/local/bin/geth
RUN chmod 0755 /usr/local/bin/geth
ENTRYPOINT ["geth"]
