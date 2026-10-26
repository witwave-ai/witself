// Package syncmodel implements the version-one synthetic synchronization model.
// It has no production callers, key custody, transport, apply driver or migration.
// Its canonical profile is frozen by the independent test encoders and vectors;
// changing normalization requires a protocol version. Model and fixture SQL proof
// do not establish cross-store synchronization or deployed authority trust.
package syncmodel
