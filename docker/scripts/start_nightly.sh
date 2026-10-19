#!/bin/sh
# URNetwork Provider Entrypoint - nightly wrapper
# All shared logic lives in provider_common.sh; this wrapper only selects the
# mode and the urnetwork_<arch>_nightly binary flavor.

PROVIDER_MODE=nightly
PROVIDER_FLAVOR=nightly
. "$(cd "$(dirname "$0")" && pwd)/provider_common.sh"

main "$@"
