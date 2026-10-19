#!/bin/sh
# URNetwork Provider Entrypoint - stable wrapper
# All shared logic lives in provider_common.sh; this wrapper only selects the
# mode and the urnetwork_<arch>_stable binary flavor.

PROVIDER_MODE=stable
PROVIDER_FLAVOR=stable
. "$(cd "$(dirname "$0")" && pwd)/provider_common.sh"

main "$@"
