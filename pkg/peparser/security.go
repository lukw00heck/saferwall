// Copyright 2020 Saferwall. All rights reserved.
// Use of this source code is governed by Apache v2 license
// license that can be found in the LICENSE file.

package pe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"

	// "encoding/hex"
	"crypto/sha256"
	"sort"
	"unsafe"

	"go.mozilla.org/pkcs7"
)

// The options for the WIN_CERTIFICATE Revision member include
// (but are not limited to) the following.
const (
	// WinCertRevision1_0 represents the WIN_CERT_REVISION_1_0 Version 1,
	// legacy version of the Win_Certificate structure.
	// It is supported only for purposes of verifying legacy Authenticode signatures
	WinCertRevision1_0 = 0x0100

	// WinCertRevision2_0 represents the WIN_CERT_REVISION_2_0. Version 2
	// is the current version of the Win_Certificate structure.
	WinCertRevision2_0 = 0x0200
)

// The options for the WIN_CERTIFICATE CertificateType member include
// (but are not limited to) the items in the following table. Note that some
// values are not currently supported.
const (
	WinCertTypeX509           = 0x0001 // Certificate contains an X.509 Certificate (Not Supported)
	WinCertTypePKCSSignedData = 0x0002 //Certificate contains a PKCS#7 SignedData structure
	WinCertTypeReserved1      = 0x0003 // Reserved
	WinCertTypeTsStackSigned  = 0x0004 // Terminal Server Protocol Stack Certificate signing (Not Supported)
)

// Certificate directory.
type Certificate struct {
	Header  WinCertificate
	Content *pkcs7.PKCS7
}

// WinCertificate encapsulates a signature used in verifying executable files.
type WinCertificate struct {
	Length          uint32 // Specifies the length, in bytes, of the signature.
	Revision        uint16 // Specifies the certificate revision.
	CertificateType uint16 // Specifies the type of certificate.
}

// Authentihash generates the pe image file hash.
// The relevant sections to exclude during hashing are:
// 	[x] The location of the checksum
//  [x] The location of the entry of the Certificate Table in the Data Directory
//	[x] The location of the Certificate Table.
func (pe *File) Authentihash() []byte {

	// Initialize a hash algorithm context.
	h := sha256.New()

	// Hash the image header from its base to immediately before the start of
	// the checksum address, as specified in Optional Header Windows-Specific Fields.
	start := uint32(0)
	optionalHeaderOffset := pe.DosHeader.Elfanew + uint32(binary.Size(pe.NtHeader))
	checksumOffset := optionalHeaderOffset + 64
	h.Write(pe.data[start:checksumOffset])

	// Skip over the checksum, which is a 4-byte field.
	start += checksumOffset + uint32(4)

	// Hash everything from the end of the checksum field to immediately before
	// the start of the Certificate Table entry, as specified in Optional Header
	// Data Directories.
	securityDirOffset := optionalHeaderOffset
	securityDirOffset += uint32(unsafe.Offsetof(pe.OptionalHeader.DataDirectory))
	securityDirOffset += uint32(binary.Size(DataDirectory{}) * ImageDirectoryEntryCertificate)
	h.Write(pe.data[start:securityDirOffset])

	// Skip over the Certificate Table entry, which is 8 bytes long.
	start = securityDirOffset + uint32(8)

	// Hash everything from the end of the Certificate Table entry to the end of
	// image header, including Section Table (headers).
	endPeHeader := uint32(len(pe.Header))
	h.Write(pe.data[start:endPeHeader])

	// Create a counter called SUM_OF_BYTES_HASHED, which is not part of the
	// signature. Set this counter to the SizeOfHeaders field, as specified in
	// Optional Header Windows-Specific Field.
	SumOfBytesHashes := pe.OptionalHeader.SizeOfHeaders

	// Build a temporary table of pointers to all of the section headers in the
	// image. The NumberOfSections field of COFF File Header indicates how big
	// the table should be. Do not include any section headers in the table
	// whose SizeOfRawData field is zero.
	sections := []ImageSectionHeader{}
	for _, section := range pe.Sections {
		if section.SizeOfRawData != 0 {
			sections = append(sections, section)
		}
	}

	// Using the PointerToRawData field (offset 20) in the referenced
	// SectionHeader structure as a key, arrange the table's elements in
	// ascending order. In other words, sort the section headers in ascending
	// order according to the disk-file offset of the sections.
	sort.Sort(byPointerToRawData(sections))

	// Walk through the sorted table, load the corresponding section into
	// memory, and hash the entire section. Use the SizeOfRawData field in the
	// SectionHeader structure to determine the amount of data to hash.
	// Add the section’s SizeOfRawData value to SUM_OF_BYTES_HASHED.
	for _, s := range sections {
		sectionData := pe.data[s.PointerToRawData : s.PointerToRawData+s.SizeOfRawData]
		SumOfBytesHashes += s.SizeOfRawData
		h.Write(sectionData)
	}

	// Create a value called FILE_SIZE, which is not part of the signature.
	// Set this value to the image’s file size, acquired from the underlying
	// file system. If FILE_SIZE is greater than SUM_OF_BYTES_HASHED, the file
	// contains extra data that must be added to the hash. This data begins at
	// the SUM_OF_BYTES_HASHED file offset, and its length is:
	// (File Size) – ((Size of AttributeCertificateTable) + SUM_OF_BYTES_HASHED)
	if pe.size > SumOfBytesHashes {
		length := pe.size - (pe.OptionalHeader.DataDirectory[ImageDirectoryEntryCertificate].Size + SumOfBytesHashes)
		extraData := pe.data[SumOfBytesHashes : SumOfBytesHashes+length]
		h.Write(extraData)
	}

	return h.Sum(nil)
}

func (pe *File) parseSecurityDirectory(rva, size uint32) (Certificate, error) {

	certHeader := WinCertificate{}
	certSize := uint32(binary.Size(certHeader))
	var pkcs *pkcs7.PKCS7

	// The virtual address value from the Certificate Table entry in the
	// Optional Header Data Directory is a file offset to the first attribute
	// certificate entry.
	fileOffset := rva

	for {
		// Boundary check
		if fileOffset+certSize > pe.size {
			log.Print("Overflow")
			return Certificate{}, errors.New("Boundary checks failed in Security Data Dir")
		}

		buf := bytes.NewReader(pe.data[fileOffset : fileOffset+certSize])
		err := binary.Read(buf, binary.LittleEndian, &certHeader)
		if err != nil {
			return Certificate{}, err
		}

		certContent := pe.data[fileOffset+certSize : fileOffset+certHeader.Length]
		pkcs, err = pkcs7.Parse(certContent)
		if err != nil {
			return Certificate{Header: certHeader}, err
		}

		// Verify the signature
		pkcs.Verify()

		// 	log.Printf("%s", hex.Dump(pkcs.Content))

		// Subsequent entries are accessed by advancing that entry's dwLength bytes,
		// rounded up to an 8-byte multiple, from the start of the current attribute
		// certificate entry.
		nextOffset := certHeader.Length + fileOffset
		nextOffset = ((nextOffset + 8 - 1) / 8) * 8

		// Check if we walked the entire table.
		if nextOffset == fileOffset+size {
			break
		}

		fileOffset = nextOffset
	}

	return Certificate{Header: certHeader, Content: pkcs}, nil
}
