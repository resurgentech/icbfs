@CHANGES@

## Windows package

`icbfs-@VERSION@-x64.msi` and `icbfs-@VERSION@-arm64.msi` install
`icbfs-winfsp` and `icbfs-cli` and a task that mounts the drive at
sign-in. [WinFsp](https://winfsp.dev/rel/) must be installed first;
the installer refuses without it. The settings are installer
properties:

```
msiexec /i icbfs-@VERSION@-x64.msi ACCOUNT=<storage account> [CONTAINER=icbfs] [FILESYSTEM=default] [DRIVELETTER=I]
```

See [docs/mount.md, "The Windows package"](https://github.com/resurgentech/icbfs/blob/@TAG@/docs/mount.md#the-windows-package)
for what it installs, where the mount gets its credential, and how to
uninstall. The Linux commands are built from source
([README](https://github.com/resurgentech/icbfs/blob/@TAG@/README.md#building)).

@SIGNED@

## Code signing policy

Free code signing provided by [SignPath.io](https://signpath.io),
certificate by [SignPath Foundation](https://signpath.org).

- **Committers and reviewers:** the [members of resurgentech](https://github.com/orgs/resurgentech/people)
- **Approvers:** the organization's owners

**Privacy:** this program will not transfer any information to other
networked systems unless specifically requested by the user or the
person installing or operating it. icbfs talks only to the storage the
user configures, and has no telemetry.
