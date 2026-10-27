#!/bin/bash
# OpenTofu for the tests' Azure storage account (test/azure/tofu/), with the
# names from .env.azure and the az CLI's login. State is local to the
# checkout and gitignored.
#
#   test/azure/tofu.sh plan
#   test/azure/tofu.sh apply
#   test/azure/tofu.sh env        # the .env.azure lines for the account it made
#   test/azure/tofu.sh <any tofu command>
#
# .env.azure (gitignored, under ICBFS_TEST_HOME) names the subscription,
# resource group, location and container; the account name is derived from
# the subscription. The az CLI must be logged in to the subscription's
# tenant as someone who can create role assignments there.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck disable=SC1091
source "$REPO/test/home.sh"
set -a
# shellcheck disable=SC1091
source "$ICBFS_TEST_HOME/.env.azure"
set +a
export TF_VAR_subscription_id="$ICBFS_TEST_AZURE_SUBSCRIPTION"
export TF_VAR_resource_group="$ICBFS_TEST_AZURE_RESOURCE_GROUP"
export TF_VAR_location="$ICBFS_TEST_AZURE_LOCATION"
export TF_VAR_container="$ICBFS_TEST_AZURE_CONTAINER"
TF=$(command -v tofu || command -v terraform)
cd "$REPO/test/azure/tofu"
case "${1:-}" in
  env) exec "$TF" output -raw env ;;
  "") echo "usage: test/azure/tofu.sh plan|apply|env|<tofu command>" >&2; exit 2 ;;
esac
"$TF" init -input=false >/dev/null
exec "$TF" "$@"
