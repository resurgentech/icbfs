#!/bin/bash
# Where the test state that isn't in git lives, for the test scripts to
# source (not run): .env.aws and .env.azure, and each VM kind's work/
# directory (the Windows VMs' password and answer ISO, downloaded images,
# logs). It's ICBFS_TEST_HOME, by default this checkout. A .env.test at
# the checkout's root (gitignored) can set it, so another checkout of
# icbfs, a submodule in a repo built on it say, shares one set of state:
#
#   ICBFS_TEST_HOME=/home/me/src/icbfs
_icbfs_checkout="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ -z "${ICBFS_TEST_HOME:-}" ] && [ -f "$_icbfs_checkout/.env.test" ]; then
  # shellcheck disable=SC1091
  . "$_icbfs_checkout/.env.test"
fi
ICBFS_TEST_HOME="${ICBFS_TEST_HOME:-$_icbfs_checkout}"
