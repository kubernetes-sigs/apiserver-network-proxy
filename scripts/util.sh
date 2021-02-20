
function check-command-installed() {
  local cmdName="${1}"

  command -v "${cmdName}" >/dev/null 2>&1 || 
  {
    echo "${cmdName} command not found. Please download dependencies using ${BASH_SOURCE%/*}/download-binaries.sh and install it in your PATH." >&2
    exit 1
  }
}