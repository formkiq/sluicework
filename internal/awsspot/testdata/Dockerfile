FROM ubuntu:24.04
RUN apt-get update -qq && apt-get install -y --no-install-recommends \
    ca-certificates curl python3 iptables iproute2 passwd sudo util-linux tar gzip \
    && rm -rf /var/lib/apt/lists/*
COPY bootstrap-probe.py /usr/local/bin/bootstrap-probe.py
COPY fake-runner.sh /usr/local/bin/fake-runner.sh
ENTRYPOINT ["python3", "/usr/local/bin/bootstrap-probe.py"]
