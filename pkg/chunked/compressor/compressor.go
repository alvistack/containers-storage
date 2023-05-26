package compressor

// NOTE: This is used from github.com/containers/image by callers that
// don't otherwise use containers/storage, so don't make this depend on any
// larger software like the graph drivers.

import (
	"bufio"
	"encoding/base64"
	"io"

	"github.com/containers/storage/pkg/chunked/internal"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	"github.com/vbatts/tar-split/archive/tar"
)

const (
	RollsumBits    = 16
	holesThreshold = int64(1 << 10)
)

type holesFinder struct {
	reader    *bufio.Reader
	zeros     int64
	threshold int64

	state int
}

const (
	holesFinderStateRead = iota
	holesFinderStateAccumulate
	holesFinderStateFound
	holesFinderStateEOF
)

// readByte reads a single byte from the underlying reader.
// If a single byte is read, the return value is (0, RAW-BYTE-VALUE, nil).
// If there are at least f.THRESHOLD consecutive zeros, then the
// return value is (N_CONSECUTIVE_ZEROS, '\x00').
func (f *holesFinder) readByte() (int64, byte, error) {
	for {
		switch f.state {
		// reading the file stream
		case holesFinderStateRead:
			if f.zeros > 0 {
				f.zeros--
				return 0, 0, nil
			}
			b, err := f.reader.ReadByte()
			if err != nil {
				return 0, b, err
			}

			if b != 0 {
				return 0, b, err
			}

			f.zeros = 1
			if f.zeros == f.threshold {
				f.state = holesFinderStateFound
			} else {
				f.state = holesFinderStateAccumulate
			}
		// accumulating zeros, but still didn't reach the threshold
		case holesFinderStateAccumulate:
			b, err := f.reader.ReadByte()
			if err != nil {
				if err == io.EOF {
					f.state = holesFinderStateEOF
					continue
				}
				return 0, b, err
			}

			if b == 0 {
				f.zeros++
				if f.zeros == f.threshold {
					f.state = holesFinderStateFound
				}
			} else {
				if err := f.reader.UnreadByte(); err != nil {
					return 0, 0, err
				}
				f.state = holesFinderStateRead
			}
		// found a hole.  Number of zeros >= threshold
		case holesFinderStateFound:
			b, err := f.reader.ReadByte()
			if err != nil {
				if err == io.EOF {
					f.state = holesFinderStateEOF
				}
				holeLen := f.zeros
				f.zeros = 0
				return holeLen, 0, nil
			}
			if b != 0 {
				if err := f.reader.UnreadByte(); err != nil {
					return 0, 0, err
				}
				f.state = holesFinderStateRead

				holeLen := f.zeros
				f.zeros = 0
				return holeLen, 0, nil
			}
			f.zeros++
		// reached EOF.  Flush pending zeros if any.
		case holesFinderStateEOF:
			if f.zeros > 0 {
				f.zeros--
				return 0, 0, nil
			}
			return 0, 0, io.EOF
		}
	}
}

type rollingChecksumReader struct {
	reader      *holesFinder
	closed      bool
	rollsum     *RollSum
	pendingHole int64

	// WrittenOut is the total number of bytes read from
	// the stream.
	WrittenOut int64

	// IsLastChunkZeros tells whether the last generated
	// chunk is a hole (made of consecutive zeros).  If it
	// is false, then the last chunk is a data chunk
	// generated by the rolling checksum.
	IsLastChunkZeros bool
}

func (rc *rollingChecksumReader) Read(b []byte) (bool, int, error) {
	rc.IsLastChunkZeros = false

	if rc.pendingHole > 0 {
		toCopy := int64(len(b))
		if rc.pendingHole < toCopy {
			toCopy = rc.pendingHole
		}
		rc.pendingHole -= toCopy
		for i := int64(0); i < toCopy; i++ {
			b[i] = 0
		}

		rc.WrittenOut += toCopy

		rc.IsLastChunkZeros = true

		// if there are no other zeros left, terminate the chunk
		return rc.pendingHole == 0, int(toCopy), nil
	}

	if rc.closed {
		return false, 0, io.EOF
	}

	for i := 0; i < len(b); i++ {
		holeLen, n, err := rc.reader.readByte()
		if err != nil {
			if err == io.EOF {
				rc.closed = true
				if i == 0 {
					return false, 0, err
				}
				return false, i, nil
			}
			// Report any other error type
			return false, -1, err
		}
		if holeLen > 0 {
			for j := int64(0); j < holeLen; j++ {
				rc.rollsum.Roll(0)
			}
			rc.pendingHole = holeLen
			return true, i, nil
		}
		b[i] = n
		rc.WrittenOut++
		rc.rollsum.Roll(n)
		if rc.rollsum.OnSplitWithBits(RollsumBits) {
			return true, i + 1, nil
		}
	}
	return false, len(b), nil
}

type chunk struct {
	ChunkOffset int64
	Offset      int64
	Checksum    string
	ChunkSize   int64
	ChunkType   string
}

func writeZstdChunkedStream(destFile io.Writer, outMetadata map[string]string, reader io.Reader, level int) error {
	// total written so far.  Used to retrieve partial offsets in the file
	dest := ioutils.NewWriteCounter(destFile)

	tr := tar.NewReader(reader)
	tr.RawAccounting = true

	buf := make([]byte, 4096)

	zstdWriter, err := internal.ZstdWriterWithLevel(dest, level)
	if err != nil {
		return err
	}
	defer func() {
		if zstdWriter != nil {
			zstdWriter.Close()
			zstdWriter.Flush()
		}
	}()

	restartCompression := func() (int64, error) {
		var offset int64
		if zstdWriter != nil {
			if err := zstdWriter.Close(); err != nil {
				return 0, err
			}
			if err := zstdWriter.Flush(); err != nil {
				return 0, err
			}
			offset = dest.Count
			zstdWriter.Reset(dest)
		}
		return offset, nil
	}

	var metadata []internal.FileMetadata
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		rawBytes := tr.RawBytes()
		if _, err := zstdWriter.Write(rawBytes); err != nil {
			return err
		}

		payloadDigester := digest.Canonical.Digester()
		chunkDigester := digest.Canonical.Digester()

		// Now handle the payload, if any
		startOffset := int64(0)
		lastOffset := int64(0)
		lastChunkOffset := int64(0)

		checksum := ""

		chunks := []chunk{}

		hf := &holesFinder{
			threshold: holesThreshold,
			reader:    bufio.NewReader(tr),
		}

		rcReader := &rollingChecksumReader{
			reader:  hf,
			rollsum: NewRollSum(),
		}

		payloadDest := io.MultiWriter(payloadDigester.Hash(), chunkDigester.Hash(), zstdWriter)
		for {
			mustSplit, read, errRead := rcReader.Read(buf)
			if errRead != nil && errRead != io.EOF {
				return err
			}
			// restart the compression only if there is a payload.
			if read > 0 {
				if startOffset == 0 {
					startOffset, err = restartCompression()
					if err != nil {
						return err
					}
					lastOffset = startOffset
				}

				if _, err := payloadDest.Write(buf[:read]); err != nil {
					return err
				}
			}
			if (mustSplit || errRead == io.EOF) && startOffset > 0 {
				off, err := restartCompression()
				if err != nil {
					return err
				}

				chunkSize := rcReader.WrittenOut - lastChunkOffset
				if chunkSize > 0 {
					chunkType := internal.ChunkTypeData
					if rcReader.IsLastChunkZeros {
						chunkType = internal.ChunkTypeZeros
					}

					chunks = append(chunks, chunk{
						ChunkOffset: lastChunkOffset,
						Offset:      lastOffset,
						Checksum:    chunkDigester.Digest().String(),
						ChunkSize:   chunkSize,
						ChunkType:   chunkType,
					})
				}

				lastOffset = off
				lastChunkOffset = rcReader.WrittenOut
				chunkDigester = digest.Canonical.Digester()
				payloadDest = io.MultiWriter(payloadDigester.Hash(), chunkDigester.Hash(), zstdWriter)
			}
			if errRead == io.EOF {
				if startOffset > 0 {
					checksum = payloadDigester.Digest().String()
				}
				break
			}
		}

		typ, err := internal.GetType(hdr.Typeflag)
		if err != nil {
			return err
		}
		xattrs := make(map[string]string)
		for k, v := range hdr.Xattrs {
			xattrs[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		entries := []internal.FileMetadata{
			{
				Type:       typ,
				Name:       hdr.Name,
				Linkname:   hdr.Linkname,
				Mode:       hdr.Mode,
				Size:       hdr.Size,
				UID:        hdr.Uid,
				GID:        hdr.Gid,
				ModTime:    &hdr.ModTime,
				AccessTime: &hdr.AccessTime,
				ChangeTime: &hdr.ChangeTime,
				Devmajor:   hdr.Devmajor,
				Devminor:   hdr.Devminor,
				Xattrs:     xattrs,
				Digest:     checksum,
				Offset:     startOffset,
				EndOffset:  lastOffset,
			},
		}
		for i := 1; i < len(chunks); i++ {
			entries = append(entries, internal.FileMetadata{
				Type:        internal.TypeChunk,
				Name:        hdr.Name,
				ChunkOffset: chunks[i].ChunkOffset,
			})
		}
		if len(chunks) > 1 {
			for i := range chunks {
				entries[i].ChunkSize = chunks[i].ChunkSize
				entries[i].Offset = chunks[i].Offset
				entries[i].ChunkDigest = chunks[i].Checksum
				entries[i].ChunkType = chunks[i].ChunkType
			}
		}
		metadata = append(metadata, entries...)
	}

	rawBytes := tr.RawBytes()
	if _, err := zstdWriter.Write(rawBytes); err != nil {
		return err
	}
	if err := zstdWriter.Flush(); err != nil {
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return err
	}
	zstdWriter = nil

	return internal.WriteZstdChunkedManifest(dest, outMetadata, uint64(dest.Count), metadata, level)
}

type zstdChunkedWriter struct {
	tarSplitOut *io.PipeWriter
	tarSplitErr chan error
}

func (w zstdChunkedWriter) Close() error {
	err := <-w.tarSplitErr
	if err != nil {
		w.tarSplitOut.Close()
		return err
	}
	return w.tarSplitOut.Close()
}

func (w zstdChunkedWriter) Write(p []byte) (int, error) {
	select {
	case err := <-w.tarSplitErr:
		w.tarSplitOut.Close()
		return 0, err
	default:
		return w.tarSplitOut.Write(p)
	}
}

// zstdChunkedWriterWithLevel writes a zstd compressed tarball where each file is
// compressed separately so it can be addressed separately.  Idea based on CRFS:
// https://github.com/google/crfs
// The difference with CRFS is that the zstd compression is used instead of gzip.
// The reason for it is that zstd supports embedding metadata ignored by the decoder
// as part of the compressed stream.
// A manifest json file with all the metadata is appended at the end of the tarball
// stream, using zstd skippable frames.
// The final file will look like:
// [FILE_1][FILE_2]..[FILE_N][SKIPPABLE FRAME 1][SKIPPABLE FRAME 2]
// Where:
// [FILE_N]: [ZSTD HEADER][TAR HEADER][PAYLOAD FILE_N][ZSTD FOOTER]
// [SKIPPABLE FRAME 1]: [ZSTD SKIPPABLE FRAME, SIZE=MANIFEST LENGTH][MANIFEST]
// [SKIPPABLE FRAME 2]: [ZSTD SKIPPABLE FRAME, SIZE=16][MANIFEST_OFFSET][MANIFEST_LENGTH][MANIFEST_LENGTH_UNCOMPRESSED][MANIFEST_TYPE][CHUNKED_ZSTD_MAGIC_NUMBER]
// MANIFEST_OFFSET, MANIFEST_LENGTH, MANIFEST_LENGTH_UNCOMPRESSED and CHUNKED_ZSTD_MAGIC_NUMBER are 64 bits unsigned in little endian format.
func zstdChunkedWriterWithLevel(out io.Writer, metadata map[string]string, level int) (io.WriteCloser, error) {
	ch := make(chan error, 1)
	r, w := io.Pipe()

	go func() {
		ch <- writeZstdChunkedStream(out, metadata, r, level)
		_, _ = io.Copy(io.Discard, r) // Ordinarily writeZstdChunkedStream consumes all of r. If it fails, ensure the write end never blocks and eventually terminates.
		r.Close()
		close(ch)
	}()

	return zstdChunkedWriter{
		tarSplitOut: w,
		tarSplitErr: ch,
	}, nil
}

// ZstdCompressor is a CompressorFunc for the zstd compression algorithm.
func ZstdCompressor(r io.Writer, metadata map[string]string, level *int) (io.WriteCloser, error) {
	if level == nil {
		l := 10
		level = &l
	}

	return zstdChunkedWriterWithLevel(r, metadata, *level)
}
