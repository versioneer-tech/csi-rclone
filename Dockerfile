FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y fuse3 ca-certificates && rm -rf /var/cache/apt/archives /var/lib/apt/lists/*
COPY rclone/ /usr/local/bin/
RUN chmod +x /usr/local/bin/*
COPY csi-rclone /csi-rclone
RUN chmod +x /csi-rclone
ENTRYPOINT ["/csi-rclone"]