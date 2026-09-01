#!/bin/bash
# docker images --format "{{.Repository}}:{{.Tag}}" | grep -w "0.1.23"|xargs -n 1 docker push
PLATFORM=linux/amd64
IMAGE_PUBLISHER=zelmar
IMAGE_VERSION="0.1.41"
PLATFORM_TAG=""
if uname -m | egrep -q 'aarch64|arm64'; then
  PLATFORM=linux/arm64
  PLATFORM_TAG="-arm64"
fi

# Two environment problems have silently broken three releases in a row. Neither
# fails in a way that points at the cause, so check for them up front.
# Set ANYDBVER_SKIP_PREFLIGHT=1 to bypass.
preflight() {
  test -n "$ANYDBVER_SKIP_PREFLIGHT" && return 0

  # Docker Desktop writes credsStore=desktop.exe into the WSL2 config. The
  # helper is a Windows binary, so every docker push dies with "exec format
  # error" long after the build has finished.
  if grep -q '"credsStore"' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; then
    echo "WARNING: credsStore is set in ${DOCKER_CONFIG:-$HOME/.docker}/config.json."
    echo "         Under WSL2 this makes every 'docker push' fail with 'exec format error'."
    echo "         Remove the credsStore key before pushing, the auths entry works on its own."
    echo
  fi

  # Cross-arch builds need the QEMU handler, and the registration does not
  # survive a WSL2 restart. Without it the build produces the wrong thing
  # instead of failing.
  local want host
  want="${PLATFORM#linux/}"
  case "$(uname -m)" in
    aarch64|arm64) host=arm64 ;;
    *)             host=amd64 ;;
  esac
  if [ "$want" != "$host" ]; then
    if ! ls /proc/sys/fs/binfmt_misc/ 2>/dev/null | grep -q qemu; then
      echo "ERROR: building $PLATFORM on $host needs QEMU binfmt, which is not registered."
      echo "       It does not survive a WSL2 restart. Re-arm it with:"
      echo "         docker run --privileged --rm tonistiigi/binfmt --install $want"
      exit 1
    fi
  fi
}
preflight
test -f ../secret/id_rsa || ssh-keygen -t rsa -f ../secret/id_rsa -P ''
cd centos7
cp ../../tools/node_ip.sh ../common/rc.local ./
docker build --platform $PLATFORM -t centos:7-sshd-systemd-$USER .
cd ../rockylinux8
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t rockylinux:8-sshd-systemd-$USER .
cd ../rockylinux9
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t rockylinux:9-sshd-systemd-$USER .
cd ../rockylinux10
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t rockylinux:10-sshd-systemd-$USER .
cd ../focal
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t ubuntu:focal-sshd-systemd-$USER .
cd ../jammy
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t ubuntu:jammy-sshd-systemd-$USER .
cd ../noble
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t ubuntu:noble-sshd-systemd-$USER .
cd ../bookworm
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t debian:bookworm-sshd-systemd-$USER .
cd ../sles15
cp ../../tools/node_ip.sh ../common/rc.local ../common/rc-local.service ./
docker build --platform $PLATFORM -t sles:15-sshd-systemd-$USER .
cd ../..
#tar --exclude=images-build --exclude=data --exclude=.git --exclude=secret --exclude=.vagrant --exclude=pkg --exclude=cmd --exclude=__pycache__  -czf images-build/ansible-anydbver/anydbver.tar.gz .
git archive --format=tar HEAD | gzip -c >images-build/ansible-anydbver/anydbver.tar.gz
cd images-build/ansible-anydbver/
docker build -t rockylinux:8-anydbver-ansible-$USER .
for img in centos:7-sshd-systemd-$USER rockylinux:8-sshd-systemd-$USER rockylinux:9-sshd-systemd-$USER rockylinux:10-sshd-systemd-$USER ubuntu:focal-sshd-systemd-$USER ubuntu:jammy-sshd-systemd-$USER ubuntu:noble-sshd-systemd-$USER debian:bookworm-sshd-systemd-$USER sles:15-sshd-systemd-$USER rockylinux:8-anydbver-ansible-$USER; do
  docker image tag $img $IMAGE_PUBLISHER/${img/$USER/$IMAGE_VERSION}$PLATFORM_TAG
done
