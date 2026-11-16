#!/usr/bin/env bash
# Let bwrap create user namespaces on a GitHub-hosted Ubuntu runner, the way
# docs/getting-started/quick-start.md sets it up, then fail the step if bwrap
# still cannot create a sandbox.
#
# Ubuntu 24.04 restricts unprivileged user namespaces through AppArmor and
# ships no profile for bwrap, so we add one that grants `userns`.
# Ubuntu 26.04 ships its own profile for /usr/bin/bwrap
# (/etc/apparmor.d/bwrap-userns-restrict). Two profiles for the same binary
# conflict and bwrap ends up with neither (issue #480). The shipped profile
# also drops all capabilities of the sandboxed programs, which breaks
# programs that build their own sandbox, such as Chromium. So we disable it
# and load ours instead.
set -euo pipefail

shipped=/etc/apparmor.d/bwrap-userns-restrict
if [ -e "$shipped" ] && [ ! -e "/etc/apparmor.d/disable/${shipped##*/}" ]; then
  echo "disabling the AppArmor profile Ubuntu ships for bwrap: $shipped"
  sudo apparmor_parser -R "$shipped"
  sudo ln -s "$shipped" /etc/apparmor.d/disable/
fi

sudo tee /etc/apparmor.d/bwrap > /dev/null <<'EOF'
abi <abi/4.0>,
/usr/bin/bwrap flags=(unconfined) {
  userns,
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/bwrap

bwrap --version
echo "kernel.apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo n/a)"
if ! bwrap --ro-bind / / -- /bin/true; then
  sudo dmesg | grep -i apparmor | tail -n 20 || true
  echo "::error::bwrap cannot create a sandbox even with /etc/apparmor.d/bwrap loaded"
  exit 1
fi
echo "bwrap can create a sandbox"
