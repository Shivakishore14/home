FROM alpine:3.20 AS config
COPY configs/victoriametrics/scrape.default.yml /scrape.yml
COPY private/ /tmp/private/
RUN if [ -f /tmp/private/monitoring/scrape.yml ]; then cp /tmp/private/monitoring/scrape.yml /scrape.yml; fi

FROM victoriametrics/victoria-metrics:v1.112.0
COPY --from=config /scrape.yml /etc/victoriametrics/scrape.yml
