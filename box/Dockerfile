FROM debian:trixie

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git curl nodejs npm \
 && corepack enable || true \
 && rm -rf /var/lib/apt/lists/*

COPY club /usr/local/bin/club
RUN chmod +x /usr/local/bin/club

WORKDIR /home/club
ENTRYPOINT ["club"]
CMD ["--help"]
