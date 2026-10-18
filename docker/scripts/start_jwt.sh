#!/bin/sh
# URNetwork Provider Entrypoint - JWT/auth-code wrapper
# All shared logic lives in provider_common.sh; this wrapper only selects the
# jwt mode and keeps the legacy AUTHCODE alias for the auth code.

# Accept AUTHCODE as an alias for URNETWORK_AUTH_CODE
if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ -n "${AUTHCODE:-}" ]; then
  export URNETWORK_AUTH_CODE="$AUTHCODE"
fi

PROVIDER_MODE=jwt
PROVIDER_FLAVOR=stable
. "$(cd "$(dirname "$0")" && pwd)/provider_common.sh"

main "$@"
