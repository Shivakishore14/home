FROM grafana/grafana:11.5.2
COPY configs/grafana/provisioning /etc/grafana/provisioning
COPY private/ /tmp/private/
USER root
RUN mkdir -p /etc/grafana/dashboards /etc/grafana/provisioning/alerting && \
    if [ -d /tmp/private/grafana/dashboards ]; then cp -r /tmp/private/grafana/dashboards/* /etc/grafana/dashboards/ 2>/dev/null || true; fi && \
    if [ -d /tmp/private/grafana/alerting ]; then cp -r /tmp/private/grafana/alerting/*.yml /etc/grafana/provisioning/alerting/ 2>/dev/null || true; fi && \
    chown -R 472:0 /etc/grafana/dashboards /etc/grafana/provisioning && \
    rm -rf /tmp/private
USER 472
