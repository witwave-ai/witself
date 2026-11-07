package trust

import (
	"bytes"
	"encoding/binary"
)

// SECURITY_DESCRIPTOR_CONTROL flags and ACCESS_ALLOWED_ACE_TYPE from WinNT.h.
// These values describe serialized bytes and do not depend on native layouts.
const (
	privateRecordSelfRelative     uint16 = 0x8000 // SE_SELF_RELATIVE
	privateRecordDACLProtected    uint16 = 0x1000 // SE_DACL_PROTECTED
	privateRecordDACLPresent      uint16 = 0x0004 // SE_DACL_PRESENT
	privateRecordAccessAllowedACE byte   = 0
)

// Parse only bounded self-relative descriptor bytes. Preserve every ACE flag
// for checkPrivateRecordPolicy, which refuses inherited and all other flags.
// The native caller supplies validated current-user and LocalSystem SID bytes.
func privateRecordDescriptorSID(b []byte, offset int) ([]byte, bool) {
	if offset < 0 || offset > len(b)-8 || b[offset] != 1 || b[offset+1] > 15 {
		return nil, false
	}
	length := 8 + int(b[offset+1])*4
	if length > len(b)-offset {
		return nil, false
	}
	return b[offset : offset+length], true
}
func privateRecordPolicyFromSecurity(b, userBytes, systemBytes []byte) (privateRecordPolicy, error) {
	p := privateRecordPolicy{}
	if len(b) < 20 || b[0] != 1 {
		return p, ReasonStateIO
	}
	control := binary.LittleEndian.Uint16(b[2:4])
	if control&privateRecordSelfRelative == 0 {
		return p, ReasonStateIO
	}
	ownerOffset := int(binary.LittleEndian.Uint32(b[4:8]))
	aclOffset := int(binary.LittleEndian.Uint32(b[16:20]))
	if ownerOffset < 20 || ownerOffset%4 != 0 || aclOffset < 20 || aclOffset%4 != 0 || aclOffset > len(b)-8 {
		return p, ReasonStateIO
	}
	owner, ok := privateRecordDescriptorSID(b, ownerOffset)
	if !ok {
		return p, ReasonStateIO
	}
	// Validate all optional offsets too, even though this query requests only
	// owner and DACL. No offset may point into the descriptor's fixed header.
	groupOffset := int(binary.LittleEndian.Uint32(b[8:12]))
	if groupOffset != 0 {
		if groupOffset < 20 || groupOffset%4 != 0 {
			return p, ReasonStateIO
		}
		if _, ok := privateRecordDescriptorSID(b, groupOffset); !ok {
			return p, ReasonStateIO
		}
	}
	if binary.LittleEndian.Uint32(b[12:16]) != 0 {
		return p, ReasonStateIO
	}
	aclSize := int(binary.LittleEndian.Uint16(b[aclOffset+2 : aclOffset+4]))
	count := int(binary.LittleEndian.Uint16(b[aclOffset+4 : aclOffset+6]))
	if (b[aclOffset] != 2 && b[aclOffset] != 4) || aclSize < 8 || aclSize > len(b)-aclOffset || count > 1024 {
		return p, ReasonStateIO
	}
	if ownerOffset < aclOffset+aclSize && ownerOffset+len(owner) > aclOffset {
		return p, ReasonStateIO
	}
	p.ownerUser = bytes.Equal(owner, userBytes)
	p.userIsSystem = bytes.Equal(userBytes, systemBytes)
	p.protected = control&privateRecordDACLProtected != 0
	p.daclPresent = control&privateRecordDACLPresent != 0
	end, pos := aclOffset+aclSize, aclOffset+8
	for range count {
		if pos > end-8 {
			return privateRecordPolicy{}, ReasonStateIO
		}
		size := int(binary.LittleEndian.Uint16(b[pos+2 : pos+4]))
		if size < 8 || size%4 != 0 || size > end-pos {
			return privateRecordPolicy{}, ReasonStateIO
		}
		ace := privateRecordACE{kind: b[pos], flags: b[pos+1], rights: binary.LittleEndian.Uint32(b[pos+4 : pos+8])}
		if ace.kind == privateRecordAccessAllowedACE {
			sid, ok := privateRecordDescriptorSID(b[pos:pos+size], 8)
			if !ok || len(sid)+8 != size {
				return privateRecordPolicy{}, ReasonStateIO
			}
			switch {
			case bytes.Equal(sid, userBytes):
				ace.principal = recordUser
			case bytes.Equal(sid, systemBytes):
				ace.principal = recordSystem
			}
		}
		p.aces = append(p.aces, ace)
		pos += size
	}
	if pos > end {
		return privateRecordPolicy{}, ReasonStateIO
	}
	p.valid = true
	return p, nil
}
