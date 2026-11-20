package tracing

// ActionIDString returns the 20-character form of a 32-byte action ID that go
// uses as the first part of a build ID (cmd/internal/buildid.HashToString):
// the first 15 bytes in URL-safe base64 without padding.
func ActionIDString(id [32]byte) string {
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	const chunks = 5
	var dst [chunks * 4]byte
	for i := range chunks {
		v := uint32(id[3*i])<<16 | uint32(id[3*i+1])<<8 | uint32(id[3*i+2])
		dst[4*i+0] = b64[(v>>18)&0x3F]
		dst[4*i+1] = b64[(v>>12)&0x3F]
		dst[4*i+2] = b64[(v>>6)&0x3F]
		dst[4*i+3] = b64[v&0x3F]
	}
	return string(dst[:])
}
