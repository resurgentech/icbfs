icbfs for Windows
=================

This package installed, under this folder:

  icbfs-winfsp.exe   mounts an icbfs filesystem as a drive (needs WinFsp)
  icbfs-cli.exe      creates and manages filesystems; named versions; trash
  mount-task.ps1     registers the sign-in task below; the installer runs it

and, when an account was given at install (msiexec /i icbfs.msi
ACCOUNT=...), a scheduled task, "icbfs mount", that mounts the drive
when a user signs in. The task runs as the signing-in user, in their
own session. It needs the user's credential for the storage account:
ICBFS_AZURE_KEY in their environment, or a token command the machine
provides (ICBFS_AZURE_TOKEN_COMMAND). The package holds no key. The
mount's log is %LOCALAPPDATA%\icbfs\icbfs-winfsp.log.

WinFsp, the file system driver icbfs-winfsp uses, is installed
separately (https://winfsp.dev/rel/); this package refuses to install
without it. WinFsp - Windows File System Proxy, Copyright (C) Bill
Zissimopoulos, https://github.com/winfsp/winfsp.

Uninstalling icbfs (Settings, Apps) removes the files, the task and the
registry key HKLM\SOFTWARE\resurgentech\icbfs. It leaves WinFsp.

Documentation: https://github.com/resurgentech/icbfs (docs/mount.md,
"The Windows package"). License: MIT (LICENSE.txt).
