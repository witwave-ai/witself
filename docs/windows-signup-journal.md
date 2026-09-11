# Windows signup journal

Native Windows checks are required for this implementation. Cross-compilation
and source checks alone do not establish supported Windows recovery.

The managed signup journal uses protected owner/DACL checks for its home,
journal, lock, temporary files, account token directories, token and config.
Existing objects must already satisfy that private-storage policy. The signup
path does not change their permissions, repair ACLs or migrate an unsafe home.
Other local account commands and an explicit `account create --out` destination
are outside this managed-storage change.

On Windows, the immediate parent of the selected `WITSELF_HOME` must already
exist. Signup can create the home and its descendants, but will reject missing
parents above it without creating them. With the default location, the user
profile is that existing parent. For a custom nested `WITSELF_HOME`, prepare its
parent directory first. POSIX keeps its existing nested-directory behavior.

That fixed parent is a durability boundary across retries. It and the created
home/owned directory chain must be flushable with the current user's ordinary
rights. Higher OS ancestors are pinned for identity using structural handles;
signup does not request write access just to pin them. The implementation requires
writable local NTFS with persistent ACLs and hard-link support, verified from
the actual handles. Reparse paths, unknown or remote volumes,
unsafe descriptors, and denied flushes fail closed.

A visible journal or lock left by an interrupted operation is not itself a
durability acknowledgement. An exact retry reestablishes its file/directory
barriers before continuing. Journal replacement retains full-record comparison
under the existing lock; token publication remains no-replace. The credential
journal remains until token and config handoff has completed durably. No denied
operation is converted to success or replaced with a directory-sync no-op.

Native acceptance must cover protected creation, adversarial descriptors,
same-volume publication, handle sharing and deletion, fixed-parent recovery,
lock-publication retry, and complete signup/re-consent/offline recovery. Tests
of API return values do not simulate power loss. The native baseline and
separate primitive/NTFS probes are required diagnostic checks and must not be
weakened to conceal an unsupported environment.
