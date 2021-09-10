package types

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/blacktop/ipsw/pkg/lzfse"
)

//go:generate stringer -type=compMethod -output decmpfs_string.go

type compMethod uint32

const (
	MAX_DECMPFS_XATTR_SIZE = 3802
	DECMPFS_MAGIC          = "cmpf" // 0x636d7066
	DECMPFS_XATTR_NAME     = "com.apple.decmpfs"
)

// https://opensource.apple.com/source/copyfile/copyfile-138/copyfile.c.auto.html
const (
	CMP_TYPE1     compMethod = 1 // Uncompressed data in xattr
	CMP_ATTR_ZLIB compMethod = 3
	CMP_RSRC_ZLIB compMethod = 4 // 64k blocks
	/*
	 *  case 5: specifies de-dup within the generation store. Don't copy decmpfs xattr.
	 *  case 6: unused
	 */
	CMP_ATTR_LZVN         compMethod = 7
	CMP_RSRC_LZVN         compMethod = 8  // 64k blocks
	CMP_ATTR_UNCOMPRESSED compMethod = 9  // uncompressed data in xattr (similar to but not identical to CMP_Type1)
	CMP_RSRC_UNCOMPRESSED compMethod = 10 // 64k chunked uncompressed data in resource fork
	CMP_ATTR_LZFSE        compMethod = 11
	CMP_RSRC_LZFSE        compMethod = 12 // 64k blocks

	/* additional types defined in AppleFSCompression project */

	CMP_MAX compMethod = 255 // Highest compression_type supported
)

// DecmpfsDiskHeader this structure represents the xattr on disk; the fields below are little-endian
type DecmpfsDiskHeader struct {
	Magic            magic
	CompressionType  compMethod
	UncompressedSize uint64
	AttrBytes        [0]byte
}

func (h DecmpfsDiskHeader) String() string {
	return fmt.Sprintf("magic=%s, compression_type=%s, uncompressed_size=%d",
		h.Magic,
		h.CompressionType,
		h.UncompressedSize,
	)
}

// DecmpfsHeader this structure represents the xattr in memory; the fields below are host-endian
type DecmpfsHeader struct {
	AttrSize         uint32
	Magic            magic
	CompressionType  uint32
	UncompressedSize uint64
	AttrBytes        [0]byte
}

// CmpfRsrcHead (fields are big-endian)
type CmpfRsrcHead struct {
	HeaderSize uint32
	TotalSize  uint32
	DataSize   uint32
	Flags      uint32
}

// cmpfRsrcBlock (1 x 64K block)
type cmpfRsrcBlock struct {
	Offset uint32
	Size   uint32
}

type CmpfRsrc struct {
	EntryCount uint32
	Entries    [32]cmpfRsrcBlock
}

type CmpfRsrcBlockHead struct {
	DataSize  uint32
	NumBlocks uint32
	Blocks    []cmpfRsrcBlock
}

type CmpfEnd struct {
	_     [24]byte
	Unk1  uint16
	Unk2  uint16
	Unk3  uint16
	Magic magic
	Flags uint32
	Size  uint64
	Unk4  uint32
}

func GetDecmpfsHeader(ne NodeEntry) (*DecmpfsDiskHeader, error) {
	var hdr DecmpfsDiskHeader
	if ne.Hdr.GetType() == APFS_TYPE_XATTR {
		if ne.Key.(j_xattr_key_t).Name == DECMPFS_XATTR_NAME {
			if err := binary.Read(bytes.NewReader(ne.Val.(j_xattr_val_t).Data.([]byte)), binary.LittleEndian, &hdr); err != nil {
				return nil, err
			}
			return &hdr, nil
		}
	}
	return nil, fmt.Errorf("type is not APFS_TYPE_XATTR")
}

// DecompressFile decompresses decmpfs data
func DecompressFile(r *io.SectionReader, decomp *bufio.Writer, hdr *DecmpfsDiskHeader) error {

	switch hdr.CompressionType {
	case CMP_ATTR_ZLIB:
		panic("CMP_ATTR_ZLIB not supported (need to figure out where to grab compressed data from)")
	case CMP_RSRC_ZLIB:
		var rsrcHdr CmpfRsrcHead
		if err := binary.Read(r, binary.BigEndian, &rsrcHdr); err != nil {
			return err
		}

		r.Seek(int64(rsrcHdr.HeaderSize), io.SeekStart)

		var blkHdr CmpfRsrcBlockHead
		if err := binary.Read(r, binary.BigEndian, &blkHdr.DataSize); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &blkHdr.NumBlocks); err != nil {
			return err
		}

		blocks := make([]cmpfRsrcBlock, blkHdr.NumBlocks)
		if err := binary.Read(r, binary.LittleEndian, &blocks); err != nil {
			return err
		}

		var n int64
		var total int64

		var max int
		for _, blk := range blocks {
			if max < int(blk.Size) {
				max = int(blk.Size)
			}
		}
		buff := make([]byte, 0, max)

		for _, blk := range blocks {
			r.Seek(int64(rsrcHdr.HeaderSize+blk.Offset+4), io.SeekStart)

			buff = buff[:blk.Size]
			if err := binary.Read(r, binary.LittleEndian, &buff); err != nil {
				return err
			}

			if buff[0] == 0x78 { // zlib block
				zr, err := zlib.NewReader(bytes.NewReader(buff))
				if err != nil {
					return fmt.Errorf("failed to create zlib reader: %v", err)
				}
				n, err = decomp.ReadFrom(zr)
				if err != nil {
					return err
				}
				zr.Close()
				total += n
			} else if (buff[0] & 0x0F) == 0x0F { // uncompressed block
				nn, err := decomp.Write(buff[1:])
				if err != nil {
					return err
				}
				total += int64(nn)
			} else {
				return fmt.Errorf("found unknown chunk type data in resource fork compressed data")
			}
		}
		// var footer CmpfEnd
		// if err := binary.Read(r, binary.BigEndian, &footer); err != nil {
		// 	return err
		// }
	case CMP_ATTR_LZVN:
		panic("CMP_ATTR_LZVN not supported (need to figure out where to grab compressed data from)")
	case CMP_RSRC_LZVN:
		fallthrough
	case CMP_RSRC_LZFSE:
		var rsrcHdr CmpfRsrcHead
		if err := binary.Read(r, binary.BigEndian, &rsrcHdr); err != nil {
			return err
		}

		r.Seek(int64(rsrcHdr.HeaderSize), io.SeekStart)

		var blkHdr CmpfRsrcBlockHead
		if err := binary.Read(r, binary.BigEndian, &blkHdr.DataSize); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &blkHdr.NumBlocks); err != nil {
			return err
		}

		blocks := make([]cmpfRsrcBlock, blkHdr.NumBlocks)
		if err := binary.Read(r, binary.LittleEndian, &blocks); err != nil {
			return err
		}

		var n int
		var total int
		var err error

		var max int
		for _, blk := range blocks {
			if max < int(blk.Size) {
				max = int(blk.Size)
			}
		}
		buff := make([]byte, 0, max)

		for _, blk := range blocks {
			r.Seek(int64(rsrcHdr.HeaderSize+blk.Offset+4), io.SeekStart)

			buff = buff[:blk.Size]
			if err := binary.Read(r, binary.LittleEndian, &buff); err != nil {
				return err
			}

			if buff[0] == 0x78 { // lzvn block
				dec, err := lzfse.NewDecoder(buff).DecodeBuffer()
				if err != nil {
					return err
				}
				// n, err = decomp.Write(lzfse.DecodeBuffer(buff))
				n, err = decomp.Write(dec)
				if err != nil {
					return err
				}
			} else if buff[0] == 0x06 { // uncompressed block TODO: make sure this is the same for lzvn AND lzfse
				n, err = decomp.Write(buff[1:])
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("found unknown chunk type data in resource fork compressed data")
			}
			total += n
		}
		// var footer CmpfEnd
		// if err := binary.Read(r, binary.BigEndian, &footer); err != nil {
		// 	return err
		// }
	default:
		return fmt.Errorf("unknown compression type: %s", hdr.CompressionType)
	}

	return nil
}
