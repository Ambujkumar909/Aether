#!/usr/bin/env bash
# The Windows app's file resources: the icon Explorer and the taskbar show for
# the .exe, and the version information in its Properties. Writes
# engine/cmd/aether-desktop/rsrc_windows_amd64.syso, which `go build` links
# in by its name. Needs windres (MinGW binutils).
#
#   bash tools/winres.sh [version]      # default 0.0.0; a release passes its own
set -euo pipefail
V=${1:-0.0.0}
[[ $V =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must be major.minor.patch" >&2; exit 2; }
cd "$(dirname "$0")/../engine/cmd/aether-desktop"
C=${V//./,},0
cat > aether.rc <<RC
1 ICON "aether.ico"
1 VERSIONINFO
FILEVERSION $C
PRODUCTVERSION $C
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904B0"
    BEGIN
      VALUE "CompanyName", "Aether"
      VALUE "FileDescription", "Aether"
      VALUE "FileVersion", "$V"
      VALUE "InternalName", "aether-desktop"
      VALUE "LegalCopyright", "Aether contributors"
      VALUE "OriginalFilename", "aether-desktop.exe"
      VALUE "ProductName", "Aether"
      VALUE "ProductVersion", "$V"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
RC
windres --target=pe-x86-64 -O coff -i aether.rc -o rsrc_windows_amd64.syso
rm aether.rc
echo "wrote rsrc_windows_amd64.syso (version $V)"
