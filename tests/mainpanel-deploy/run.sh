#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"

python3 -m unittest discover -s tests/mainpanel-deploy -p 'test_*.py' -v
tests/mainpanel-deploy/test_github_preflight.sh
tests/mainpanel-deploy/test_github_remote_transaction.sh

for script in scripts/mainpanel-deploy/*.sh tests/mainpanel-deploy/*.sh; do
  bash -n "$script"
done

printf 'mainpanel-deploy tests: OK\n'
