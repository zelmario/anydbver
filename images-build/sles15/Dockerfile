FROM         registry.suse.com/bci/bci-base:15.7
 ENV container docker
 # Everything below comes from the free SLE_BCI repo that ships enabled in the
 # base image, so no SUSE subscription is needed to build or run this.
 RUN zypper -n --gpg-auto-import-keys refresh; \
  zypper -n install --no-recommends systemd openssh-server openssh-clients \
    rsync python3 iproute2 procps sudo curl tar gzip \
    gawk hostname which; \
  if uname -m | grep x86_64 ; then curl -LO  https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 ; chmod +x yq_linux_amd64; mv yq_linux_amd64 /usr/local/bin/yq ; \
  else curl -LO  https://github.com/mikefarah/yq/releases/latest/download/yq_linux_arm64 ; chmod +x yq_linux_arm64; mv yq_linux_arm64 /usr/local/bin/yq ; fi ; \
 zypper clean -a; ssh-keygen -A; systemctl enable sshd
 STOPSIGNAL SIGRTMIN+3
 EXPOSE 22
 RUN for service in\
 console-getty.service\
 dbus.service\
 dbus.socket\
 dev-hugepages.mount\
 getty.target\
 sys-fs-fuse-connections.mount\
 systemd-logind.service\
 systemd-remount-fs.service\
 systemd-udevd\
 systemd-vconsole-setup.service\
 ;do systemctl mask $service; done; mkdir -p /root/.ssh
 COPY node_ip.sh /usr/bin/node_ip.sh
 COPY rc.local /etc/rc.local
 COPY rc-local.service /etc/systemd/system/rc-local.service
 RUN chown root:root /etc/rc.local; chmod 0755 /etc/rc.local ; systemctl enable rc-local
 VOLUME [ "/sys/fs/cgroup" ]
 CMD [ "/usr/lib/systemd/systemd", "--system", "--unit=multi-user.target", "--log-target=console", "--log-level=debug", "--show-status=true" ]
