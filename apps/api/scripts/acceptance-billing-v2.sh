#!/usr/bin/env bash
# Repeatable Billing V2 acceptance run.
#
# The suite writes organizations, leases, ledger rows and debts, so it MUST
# point at a disposable test database with the migrations applied — never at
# production. Nothing here calls a production HTTP endpoint.
#
#   ROOST_TEST_DATABASE_URL='postgres://.../postgres' \
#   ROOST_ACCEPTANCE_CONFIRM=YES \
#     ./scripts/acceptance-billing-v2.sh
#
# Optional:
#   ROOST_ACCEPTANCE_PRICE_BOOK=<file-or-url>  also verify the price-book hash
#   GO=<path-to-go>                            non-default go binary
set -euo pipefail

cd "$(dirname "$0")/.."

GO="${GO:-go}"
fail() { printf 'acceptance: %s\n' "$*" >&2; exit 1; }

[ -n "${ROOST_TEST_DATABASE_URL:-}" ] || fail "set ROOST_TEST_DATABASE_URL to a disposable test database"
[ "${ROOST_ACCEPTANCE_CONFIRM:-}" = "YES" ] || \
  fail "set ROOST_ACCEPTANCE_CONFIRM=YES to acknowledge this writes to the test database"

# Prints the host without credentials so the operator can see the target.
db_host="$(python3 - "$ROOST_TEST_DATABASE_URL" <<'PY'
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
print(u.hostname or sys.argv[1])
PY
)"
printf 'acceptance: target database host = %s\n' "$db_host"

case "$(printf '%s' "$db_host" | tr '[:upper:]' '[:lower:]')" in
  *production*|*prod-db*) fail "refusing a host that looks like production: $db_host" ;;
esac

printf '\n== 1/3 store-level Billing V2 acceptance ==\n'
ROOST_TEST_DATABASE_URL="$ROOST_TEST_DATABASE_URL" \
  "$GO" test ./internal/billing -run '^TestBillingV2Acceptance$' -count=1 -v

printf '\n== 2/3 HTTP error mapping (409 price change, 400 non-UUID ids) ==\n'
"$GO" test ./internal/httpapi \
  -run 'TestRelaySettleV2ErrorCodes|TestLeaseHandlersRejectNonUUIDIDs|TestRunHandlersRejectNonUUIDOrgID|TestRelayHandlersRejectNonUUIDReservation|TestUUIDParseErrorMapsToClientError' \
  -count=1 -v

if [ -n "${ROOST_ACCEPTANCE_PRICE_BOOK:-}" ]; then
  printf '\n== 3/3 price-book canonical hash ==\n'
  python3 scripts/check-price-hash.py --book "$ROOST_ACCEPTANCE_PRICE_BOOK"
else
  printf '\n== 3/3 price-book hash skipped (set ROOST_ACCEPTANCE_PRICE_BOOK to enable) ==\n'
fi

cat <<'EOF'

acceptance: PASS
Checklist (see docs/ACCEPTANCE-BILLING-V2.md):
  [x] same model+usage through relay and dslpha differs by <= 1 credit
  [x] expired reservation still settles for its usage
  [x] insufficient balance is a partial debit plus a debt row
  [x] changed price version is rejected (HTTP 409) before money moves
  [x] compression reports a positive net gain and lowers the charge
EOF
