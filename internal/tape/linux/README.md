# Physical read-only module

Every physical operation requires explicit local read-only and write-protection confirmation, a configured non-rewinding node, matching expected WWID, and an approved profile file. The profile approval is an operator-maintained qualification record. All current repository hardware gates remain pending.

`AcquireReadOnly` takes an interprocess advisory lock before inspecting the configured device. It checks Linux st major/minor mapping, the configured sysfs node's device number and WWID, model/vendor/firmware, kernel release, mt-st version, variable block mode, online state and observed write protection. It never enumerates other devices. Re-identification precedes motion and reader acquisition. Pinned device-node handles prevent ordinary pathname retargeting from silently selecting a different inode.

The module allows only rewind, forward-file positioning and unload through fixed mt-st arguments. Record reads use one retained read-only descriptor. Standard read-only MTIOCGET status on that descriptor distinguishes zero-read filemarks and EOD. Unknown flags, unknown required positions, I/O failures and mismatched record sizes fail closed. EIO is never interpreted as EOD. Physical writer and append methods always reject access.

The MTIOCGET structure is supported on Linux amd64 and arm64 LP64. Kernel tape I/O can remain blocked despite context cancellation. A blocked operation retains the lease until the call and descriptor close finish. Operator intervention may be required. Advisory locks also require other host tape tools to honor the exclusive operator session. These limitations must be qualified on the intended host. [Linux st interface](https://man7.org/linux/man-pages/man4/st.4.html), [Linux mtio UAPI](https://github.com/torvalds/linux/blob/master/include/uapi/linux/mtio.h), [mt-st command implementation](https://github.com/iustin/mt-st/blob/master/mt.c)

A read profile has this strict shape. The values below remain synthetic and cannot qualify a real device.

```json
{
  "id": "synthetic-only",
  "identity": "synthetic-wwid",
  "vendor": "TEST",
  "model": "SYNTHETIC",
  "firmware": "1",
  "kernel_release": "synthetic-kernel",
  "mt_st_version": "1.7",
  "block_mode": "variable",
  "max_record_bytes": 262144,
  "exact_record_bytes": 262144,
  "filemark_policy": "zero-read-with-gmt-eof",
  "eod_policy": "zero-read-with-gmt-eod",
  "read_only_approved": false,
  "qualification_ref": "pending operator-supervised qualification"
}
```

An explicitly qualified legacy profile may use `exact_record_bytes` zero to capture variable record lengths, each a multiple of 512 bytes up to the bounded maximum. The read buffer must accommodate that maximum. Managed format readers require their exact format record size. The profile file must be a regular file with no group/other write permission. Lock and snapshot parent directories must be private existing directories. Native and container operators must use the same host lock directory and backend UID.

`Snapshot` rewinds under the lease, copies exact returned record bytes into numbered ordinary files, and records observed hashes. It checks file/byte limits and disk reserve, fsyncs files and directories, checks reader close and identity, and publishes by atomic no-replace rename. The exact target is a configured absent legacy-root directory. Its private partial sibling is retained after failure for read-only investigation. No extraction occurs. A successful capture establishes neither source completeness nor independent archive verification. The existing managed/legacy validators and restore pipeline assess the captured files.

All tests use synthetic filesystem trees and fake devices or mt operations. The interprocess test uses a second test process with the same synthetic lock. GNU tar independently reads the captured fixture files. No test accesses real sysfs, opens a physical device or runs a tape-control utility against hardware.
