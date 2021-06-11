# wal
---
written by AlexStocks on 20210525

```go

func walName(seq, index uint64) string {
	return fmt.Sprintf("%016x-%016x.wal", seq, index)
}

// wal/wal.go
func Create(dirpath string, metadata []byte) (*WAL, error) {
	// keep temporary wal directory so WAL initialization appears atomic
	tmpdirpath := filepath.Clean(dirpath) + ".tmp"
    fileutil.CreateDirAll(tmpdirpath)
	p := filepath.Join(tmpdirpath, walName(0, 0)) // 创建 dirpath.tmp/0-0.wal
	f := fileutil.LockFile(p, os.O_WRONLY|os.O_CREATE, fileutil.PrivateFileMode)
    f.Seek(0, io.SeekEnd)
	// 给 wal 文件预分配 64MiB 
    fileutil.Preallocate(f.File, SegmentSizeBytes, true)
	w := &WAL{
		lg:       lg,
		dir:      dirpath,
		metadata: metadata,
	}
	w.encoder, err = newFileEncoder(f.File, 0)
	w.locks = append(w.locks, f)
    w.saveCrc(0)
    w.encoder.encode(&walpb.Record{Type: metadataType, Data: metadata})
    w.SaveSnapshot(walpb.Snapshot{})
    w.renameWAL(tmpdirpath)
    // directory was renamed; sync parent dir to persist rename
    pdir := fileutil.OpenDir(filepath.Dir(w.dir))
    fileutil.Fsync(pdir)
    pdir.Close()
    return w, nil
}
```

## wal 存储的内容

```go
message Record {
	optional int64 type  = 1 [(gogoproto.nullable) = false];
	optional uint32 crc  = 2 [(gogoproto.nullable) = false];
	optional bytes data  = 3;
}

message Snapshot {
	optional uint64 index = 1 [(gogoproto.nullable) = false];
	optional uint64 term  = 2 [(gogoproto.nullable) = false];
}
```

### encoder

```go
// 8 * 512 = 4k
// 可见一个 encoder 一次向一个文件写入 4k 数据
const walPageBytes = 8 * minSectorSize

type encoder struct {
	mu sync.Mutex // bw lock
	bw *ioutil.PageWriter

	crc       hash.Hash32 //  常驻 crc，减少资源分配耗费 
	buf       []byte // 常用编码 buffer，减少内存分配
	uint64buf []byte // 一个 uint64 编码 buffer 
}

// 创建 encoder，wal 不会直接调用该函数，而是通过 newFileEncoder 调用这个函数
func newEncoder(w io.Writer, prevCrc uint32, pageOffset int) *encoder {
	return &encoder{
		bw:  ioutil.NewPageWriter(w, walPageBytes, pageOffset),
		crc: crc.New(prevCrc, crcTable),
		// 1MB buffer
		buf:       make([]byte, 1024*1024),
		uint64buf: make([]byte, 8),
	}
}

// 真正被 wal 使用的接口是 newFileEncoder 函数，从 @f 当前位置开始写入 record
func newFileEncoder(f *os.File, prevCrc uint32) (*encoder, error) {
	offset := f.Seek(0, io.SeekCurrent) // 从文件当前位置开始往后写数据
	return newEncoder(f, prevCrc, int(offset)), nil
}
```

写数据流程

```go

func (e *encoder) encode(rec *walpb.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.crc.Write(rec.Data)
	rec.Crc = e.crc.Sum32()
	var (
		data []byte
		err  error
		n    int
	)

	if rec.Size() > len(e.buf) {
        // 数据量大于 1MB，不使用 encoder 的 buffer 
		data = rec.Marshal()
	} else {
        // 使用 encoder 的 buffer 进行编码
		n = rec.MarshalTo(e.buf)
		data = e.buf[:n]
	}
    // 进行 8B 对齐，lenField 是对齐后的长度，padBytes 是需要对 data 进行 padding 的数据长度
	lenField, padBytes := encodeFrameSize(len(data))
	writeUint64(e.bw, lenField, e.uint64buf)
    // 补充 padding 数据 
	if padBytes != 0 {
		data = append(data, make([]byte, padBytes)...)
	}
    // 把数据写入 wal 文件
	n, err = e.bw.Write(data)
	return err
}

```

## filePipeline

预先创建 wal 文件。使用时调用 Open 接口即可。

```go
// filePipeline pipelines allocating disk space
type filePipeline struct {
	lg *zap.Logger

	// dir to put files
	dir string
	// size of files to make, in bytes
	size int64
	// count number of files generated
	count int

	filec chan *fileutil.LockedFile
	errc  chan error
	donec chan struct{}
}

func newFilePipeline(lg *zap.Logger, dir string, fileSize int64) *filePipeline {
	fp := &filePipeline{
		lg:    lg,
		dir:   dir,
		size:  fileSize,
		filec: make(chan *fileutil.LockedFile),
		errc:  make(chan error, 1),
		donec: make(chan struct{}),
	}
	go fp.run()
	return fp
}
func (fp *filePipeline) alloc() (f *fileutil.LockedFile, err error) {
	// count % 2 so this file isn't the same as the one last published
	fpath := filepath.Join(fp.dir, fmt.Sprintf("%d.tmp", fp.count%2))
	f = fileutil.LockFile(fpath, os.O_CREATE|os.O_WRONLY, fileutil.PrivateFileMode)
	fileutil.Preallocate(f.File, fp.size, true)
	fp.count++
	return f, nil
}

func (fp *filePipeline) run() {
	for {
		f := fp.alloc()
		select {
		case fp.filec <- f:
	}
}

// Open returns a fresh file for writing. Rename the file before calling
// Open again or there will be file collisions.
func (fp *filePipeline) Open() (f *fileutil.LockedFile, err error) {
	select {
	case f = <-fp.filec:
	case err = <-fp.errc:
	}
	return f, err
}
```





