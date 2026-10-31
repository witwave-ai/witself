# TEST-ONLY public fixtures

These five RSA-3072 keypairs are public test fixtures with no authority. Their private keys are intentionally committed. They must never be used for production signing, and no fixture key ID may occur in ProductionRoots. Filenames and PEM block types carry TEST-ONLY. The public DER is SubjectPublicKeyInfo and the private DER is PKCS#8. Ordinary tests never generate keys.
