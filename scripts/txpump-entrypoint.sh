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
    if [ -z "${TXPUMP_DELEGATION_POOL_KEY_HASH:-}" ]; then
      pool_key="/configs/keys/cold.vkey"
      if [ ! -f "${pool_key}" ]; then
        echo "txpump: delegation pool key is missing" >&2
        exit 1
      fi
      TXPUMP_DELEGATION_POOL_KEY_HASH="$(cardano-cli latest stake-pool id --cold-verification-key-file "${pool_key}" --output-format hex)"
      export TXPUMP_DELEGATION_POOL_KEY_HASH
    fi
    ;;
esac

mkdir -p "${log_dir}"
if [ "$(id -u)" = "0" ]; then
  # Connecting to a Unix socket needs write permission on it, and a node may
  # create its socket owned by root with mode 0755, possibly after this
  # script starts. Unless the txpump user is known to be able to write to the
  # socket, stay root, with the log file created up front and owned by
  # txpump so the analyzer can still read it.
  node_addr="${TXPUMP_NODE_ADDR:-/ipc/node.socket}"
  stay_root=""
  case "${node_addr}" in
    /*)
      if [ ! -S "${node_addr}" ]; then
        stay_root="does not exist yet"
      elif ! setpriv --reuid=txpump --regid=txpump --clear-groups \
        sh -c 'test -w "$1"' sh "${node_addr}"; then
        stay_root="is not writable by the txpump user"
      fi
      ;;
  esac
  if [ -n "${stay_root}" ]; then
    echo "txpump: ${node_addr} ${stay_root}; running as root" >&2
    log_file="${log_dir}/txpump.log"
    # The log directory is writable by txpump, so never follow a link planted
    # at the log path while privileged.
    if [ -L "${log_file}" ]; then
      echo "txpump: ${log_file} is a symbolic link; refusing to start" >&2
      exit 1
    fi
    (set -C; : > "${log_file}") 2>/dev/null || true
    if [ -L "${log_file}" ]; then
      echo "txpump: ${log_file} is a symbolic link; refusing to start" >&2
      exit 1
    fi
    chown -R txpump:txpump "${log_dir}"
    chmod 0640 "${log_file}"
    exec /bin/txpump
  fi
  chown -R txpump:txpump "${log_dir}"
  exec su -s /bin/sh txpump -c 'exec /bin/txpump'
fi
exec /bin/txpump
