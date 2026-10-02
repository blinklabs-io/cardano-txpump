#!/usr/bin/env sh
set -eu

# The analyzer image uses uid 1000. Running txpump with the same uid keeps its
# 0640 structured log readable through the shared volume.

log_dir="${TXPUMP_LOG_DIR:-/logs}"
types="${TXPUMP_TYPES:-payment,delegation,governance,plutus}"
types="$(printf '%s' "${types}" | tr -d '[:space:]')"

# txpump uses its funding keys as stake credentials, so only delegation runs
# need the pool key hash.
case ",${types}," in
  *,delegation,*)
    pool_key="/configs/keys/cold.vkey"
    if [ ! -f "${pool_key}" ]; then
      echo "txpump: delegation pool key is missing" >&2
      exit 1
    fi
    TXPUMP_DELEGATION_POOL_KEY_HASH="$(cardano-cli latest stake-pool id --cold-verification-key-file "${pool_key}" --output-format hex)"
    export TXPUMP_DELEGATION_POOL_KEY_HASH
    ;;
esac

mkdir -p "${log_dir}"
if [ "$(id -u)" = "0" ]; then
  chown -R txpump:txpump "${log_dir}"
  exec su -s /bin/sh txpump -c 'exec /bin/txpump'
fi
exec /bin/txpump
