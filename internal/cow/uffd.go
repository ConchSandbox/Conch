package cow

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unsafe"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

const (
	uffdEventPagefault     = byte(0x12)
	uffdMessageSize        = 32
	uffdReadyACK           = byte(1)
	uffdPagefaultFlagWrite = uint64(1 << 0)
	uffdioWakeRequest      = uintptr(0x8010aa02)
	linuxUnixPathMax       = 107
	uffdBindAttempts       = 8
)

type uffdRange struct {
	HVA          uint64
	Length       uint64
	GuestOffset  uint64
	HostPageSize uint64
}

type uffdOperations struct {
	wake          func(fd int, start, length uint64) error
	poll          func(fds []unix.PollFd, timeout int) (int, error)
	read          func(fd int, buffer []byte) (int, error)
	acceptTimeout time.Duration
	pollTimeout   int
}

func productionUFFDOperations() uffdOperations {
	return uffdOperations{
		wake:          wakeUFFDRange,
		poll:          unix.Poll,
		read:          unix.Read,
		acceptTimeout: defaultUFFDAcceptTimeout,
		pollTimeout:   100,
	}
}

type directUFFDMapping struct {
	BaseHVA       uint64 `json:"base_host_virt_addr"`
	Size          uint64 `json:"size"`
	Offset        uint64 `json:"offset"`
	PageSizeBytes uint64 `json:"page_size_kib"`
}

type wrappedUFFDMappings struct {
	Mappings []wrappedUFFDMapping `json:"mappings"`
}

type wrappedUFFDMapping struct {
	BaseHVA  uint64 `json:"base-host-virt-addr"`
	Size     uint64 `json:"size"`
	Offset   uint64 `json:"offset"`
	PageSize uint64 `json:"page-size"`
}

func receiveUFFDHandoff(conn *net.UnixConn, memorySize, blockSize uint64) (*os.File, []uffdRange, error) {
	return receiveUFFDHandoffForHostPage(conn, memorySize, blockSize, uint64(os.Getpagesize()))
}

func receiveUFFDHandoffForHostPage(conn *net.UnixConn, memorySize, blockSize, hostPageSize uint64) (*os.File, []uffdRange, error) {
	if err := conn.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
		return nil, nil, fmt.Errorf("set UFFD handoff deadline: %w", err)
	}
	firstPayload := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(2*4))
	read, oobRead, flags, _, err := conn.ReadMsgUnix(firstPayload, oob)
	if err != nil {
		return nil, nil, fmt.Errorf("read UFFD handoff: %w", err)
	}
	fds, err := parseFDs(oob[:oobRead])
	if err != nil {
		return nil, nil, err
	}
	if flags&unix.MSG_CTRUNC != 0 {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("UFFD handoff ancillary data truncated")
	}
	if flags&unix.MSG_TRUNC != 0 {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("UFFD handoff first payload truncated")
	}
	if len(fds) != 1 {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("expected exactly one UFFD descriptor, got %d", len(fds))
	}
	payload, err := readOneUFFDJSONValue(conn, firstPayload[:read])
	if err != nil {
		closeFDs(fds)
		return nil, nil, err
	}
	ranges, err := decodeUFFDRanges(payload, memorySize, blockSize, hostPageSize)
	if err != nil {
		closeFDs(fds)
		return nil, nil, err
	}
	uffd := os.NewFile(uintptr(fds[0]), "stratovirt-uffd")
	if uffd == nil {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("wrap UFFD descriptor")
	}
	return uffd, ranges, nil
}

type countingReader struct {
	reader io.Reader
	read   int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	reader.read += int64(read)
	return read, err
}

func readOneUFFDJSONValue(conn *net.UnixConn, first []byte) ([]byte, error) {
	if len(first) > maxFrameSize {
		return nil, fmt.Errorf("UFFD handoff payload too large")
	}
	firstReader := bytes.NewReader(first)
	connectionReader := &countingReader{reader: conn}
	limited := io.LimitReader(io.MultiReader(firstReader, connectionReader), maxFrameSize+1)
	decoder := json.NewDecoder(limited)
	var payload json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("stream UFFD mappings: %w", err)
	}
	if int64(len(first))+connectionReader.read > int64(maxFrameSize) || len(payload) > maxFrameSize {
		return nil, fmt.Errorf("UFFD handoff payload too large")
	}
	buffered, err := io.ReadAll(decoder.Buffered())
	if err != nil {
		return nil, fmt.Errorf("inspect UFFD mapping suffix: %w", err)
	}
	unreadFirst, err := io.ReadAll(firstReader)
	if err != nil {
		return nil, fmt.Errorf("inspect initial UFFD mapping suffix: %w", err)
	}
	if len(bytes.TrimSpace(buffered)) != 0 || len(bytes.TrimSpace(unreadFirst)) != 0 {
		return nil, fmt.Errorf("multiple JSON values in UFFD handoff")
	}
	return payload, nil
}

func decodeUFFDRanges(payload []byte, memorySize, blockSize, hostPageSize uint64) ([]uffdRange, error) {
	if memorySize == 0 || blockSize == 0 || hostPageSize == 0 {
		return nil, fmt.Errorf("invalid attachment memory geometry")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty UFFD mapping payload")
	}
	var ranges []uffdRange
	if trimmed[0] == '[' {
		var mappings []directUFFDMapping
		if err := decodeStrictPayload(trimmed, &mappings); err != nil {
			return nil, fmt.Errorf("decode direct UFFD mappings: %w", err)
		}
		for _, mapping := range mappings {
			ranges = append(ranges, uffdRange{HVA: mapping.BaseHVA, Length: mapping.Size, GuestOffset: mapping.Offset, HostPageSize: mapping.PageSizeBytes})
		}
	} else {
		var wrapped wrappedUFFDMappings
		if err := decodeStrictPayload(trimmed, &wrapped); err != nil {
			return nil, fmt.Errorf("decode wrapped UFFD mappings: %w", err)
		}
		for _, mapping := range wrapped.Mappings {
			ranges = append(ranges, uffdRange{HVA: mapping.BaseHVA, Length: mapping.Size, GuestOffset: mapping.Offset, HostPageSize: mapping.PageSize})
		}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("UFFD mappings are empty")
	}
	nextGuest := uint64(0)
	previousHVAEnd := uint64(0)
	for index, item := range ranges {
		if item.HostPageSize != hostPageSize || !compatibleHostPageSize(item.HostPageSize, blockSize) || item.Length == 0 ||
			item.HVA%item.HostPageSize != 0 || item.Length%item.HostPageSize != 0 || item.GuestOffset%item.HostPageSize != 0 {
			return nil, fmt.Errorf("UFFD mapping %d has invalid geometry", index)
		}
		if item.HVA > ^uint64(0)-item.Length || item.GuestOffset > memorySize || item.Length > memorySize-item.GuestOffset {
			return nil, fmt.Errorf("UFFD mapping %d overflows its address space", index)
		}
		if index != 0 && item.HVA < previousHVAEnd {
			return nil, fmt.Errorf("UFFD HVA mappings are unsorted or overlapping")
		}
		if item.GuestOffset != nextGuest {
			return nil, fmt.Errorf("UFFD Guest mappings are not a sorted contiguous cover")
		}
		previousHVAEnd = item.HVA + item.Length
		nextGuest = item.GuestOffset + item.Length
	}
	if nextGuest != memorySize {
		return nil, fmt.Errorf("UFFD Guest mappings do not cover memory")
	}
	return ranges, nil
}

func decodeStrictPayload(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func (server *Server) prepareUFFDHandoff(item *attachment, response Response) Response {
	directory := filepath.Dir(server.socketPath)
	var listener *net.UnixListener
	var socketPath string
	for range uffdBindAttempts {
		server.mu.Lock()
		server.uffdSequence++
		sequence := server.uffdSequence
		server.mu.Unlock()
		candidate, err := uffdSocketCandidate(directory, item.token, sequence)
		if err != nil {
			response.Error = err.Error()
			return response
		}
		listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: candidate, Net: "unix"})
		if err == nil {
			socketPath = candidate
			break
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			response.Error = fmt.Sprintf("listen UFFD socket: %v", err)
			return response
		}
	}
	if listener == nil {
		response.Error = "unable to bind generated UFFD socket"
		return response
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		response.Error = fmt.Sprintf("chmod UFFD socket: %v", err)
		return response
	}
	server.mu.Lock()
	if server.closed || server.attachments[item.token] != item || item.handoffPrepared {
		server.mu.Unlock()
		_ = listener.Close()
		_ = os.Remove(socketPath)
		response.Error = "attachment changed during UFFD preparation"
		return response
	}
	item.handoffPrepared = true
	item.uffdListener = listener
	item.uffdSocketPath = socketPath
	item.handoffDone = make(chan struct{})
	server.mu.Unlock()
	go server.acceptUFFDHandoff(item, listener, socketPath)
	response.OK = true
	response.UFFDSocketPath = socketPath
	return response
}

func uffdSocketCandidate(directory, token string, sequence uint64) (string, error) {
	digest := sha256.Sum256([]byte(token))
	encoded := base64.RawURLEncoding.EncodeToString(digest[:12])
	path := filepath.Join(directory, "u-"+encoded+"-"+strconv.FormatUint(sequence, 36))
	if len(path) > linuxUnixPathMax {
		return "", fmt.Errorf("UFFD Unix socket path is %d bytes; maximum is %d", len(path), linuxUnixPathMax)
	}
	return path, nil
}

func (server *Server) acceptUFFDHandoff(item *attachment, listener *net.UnixListener, socketPath string) {
	server.mu.Lock()
	done := item.handoffDone
	server.mu.Unlock()
	var handoffErr error
	defer func() {
		server.mu.Lock()
		if server.attachments[item.token] == item {
			item.handoffErr = handoffErr
		}
		server.mu.Unlock()
		close(done)
	}()
	defer listener.Close()
	defer os.Remove(socketPath)
	_ = listener.SetDeadline(time.Now().Add(server.uffdOps.acceptTimeout))
	conn, err := listener.AcceptUnix()
	if err != nil {
		handoffErr = fmt.Errorf("accept UFFD handoff: %w", err)
		return
	}
	defer conn.Close()
	uffd, ranges, err := receiveUFFDHandoff(conn, item.pinned.Manifest.MemorySize, item.pinned.Manifest.BlockSize)
	if err != nil {
		handoffErr = err
		return
	}
	if _, err := conn.Write([]byte{uffdReadyACK}); err != nil {
		_ = uffd.Close()
		handoffErr = fmt.Errorf("acknowledge UFFD handoff: %w", err)
		return
	}
	server.mu.Lock()
	if server.attachments[item.token] != item || item.uffd != nil {
		server.mu.Unlock()
		_ = uffd.Close()
		handoffErr = fmt.Errorf("attachment changed during UFFD handoff")
		return
	}
	item.uffd = uffd
	item.uffdRanges = ranges
	item.workerStop = make(chan struct{})
	item.workerDone = make(chan struct{})
	go server.serveUFFD(item, uffd, ranges, item.workerStop, item.workerDone)
	server.mu.Unlock()
}

func (server *Server) serveUFFD(item *attachment, uffd *os.File, ranges []uffdRange, stop, done chan struct{}) {
	defer close(done)
	defer func() {
		_ = uffd.Close()
		server.mu.Lock()
		if item.uffd == uffd {
			item.uffd = nil
		}
		server.mu.Unlock()
	}()
	if err := server.runUFFD(uffd, item, ranges, stop); err != nil {
		logger := ulog.GetLogger()
		logger.Error("UFFD worker failed", ulog.F("sandbox_id", item.sandboxID), ulog.F("error", err))
	}
}

func (server *Server) runUFFD(uffd *os.File, item *attachment, ranges []uffdRange, stop <-chan struct{}) error {
	fd := int(uffd.Fd())
	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	message := make([]byte, uffdMessageSize)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		count, err := server.uffdOps.poll(pollFDs, server.uffdOps.pollTimeout)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("poll UFFD: %w", err)
		}
		if count == 0 {
			continue
		}
		if pollFDs[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("UFFD poll failed with events %#x", pollFDs[0].Revents)
		}
		if pollFDs[0].Revents&unix.POLLIN == 0 {
			continue
		}
		read, err := server.uffdOps.read(fd, message)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return fmt.Errorf("read UFFD message: %w", err)
		}
		if read != len(message) {
			return fmt.Errorf("read UFFD message: got %d bytes, want %d", read, len(message))
		}
		if err := server.serviceUFFDMessage(item, fd, ranges, message); err != nil {
			return fmt.Errorf("service UFFD message: %w", err)
		}
	}
}

func (server *Server) serviceUFFDMessage(item *attachment, uffd int, ranges []uffdRange, message []byte) error {
	if len(message) != uffdMessageSize {
		return fmt.Errorf("invalid UFFD message size %d", len(message))
	}
	if message[0] != uffdEventPagefault {
		return nil
	}
	flags := binary.LittleEndian.Uint64(message[8:16])
	if flags&^uffdPagefaultFlagWrite != 0 {
		return fmt.Errorf("unsupported non-missing UFFD pagefault flags %#x", flags)
	}
	return server.handlePageFault(item, uffd, binary.LittleEndian.Uint64(message[16:24]), ranges)
}

func (server *Server) handlePageFault(item *attachment, uffd int, faultAddress uint64, ranges []uffdRange) error {
	blockSize := item.pinned.Manifest.BlockSize
	if blockSize == 0 || blockSize > uint64(^uint(0)>>1) {
		return fmt.Errorf("unsupported fault block size %d", blockSize)
	}
	var faultPage, guestOffset, hostPageSize uint64
	for _, candidate := range ranges {
		if faultAddress >= candidate.HVA && faultAddress-candidate.HVA < candidate.Length {
			hostPageSize = candidate.HostPageSize
			pageOffset := faultAddress - candidate.HVA
			pageOffset -= pageOffset % hostPageSize
			faultPage = candidate.HVA + pageOffset
			guestOffset = candidate.GuestOffset + pageOffset
			break
		}
	}
	if hostPageSize == 0 || !compatibleHostPageSize(hostPageSize, blockSize) || hostPageSize > item.pinned.Manifest.MemorySize || guestOffset > item.pinned.Manifest.MemorySize-hostPageSize {
		return fmt.Errorf("fault address %#x is outside registered Guest memory", faultAddress)
	}
	page := make([]byte, int(hostPageSize))
	for offset := uint64(0); offset < hostPageSize; offset += blockSize {
		if err := item.pinned.ReadPage(guestOffset+offset, page[int(offset):int(offset+blockSize)]); err != nil {
			return fmt.Errorf("read sparse page at %#x: %w", guestOffset+offset, err)
		}
	}
	written, err := item.memfd.WriteAt(page, int64(guestOffset))
	if err != nil {
		return fmt.Errorf("write memfd page at %#x: %w", guestOffset, err)
	}
	if written != len(page) {
		return fmt.Errorf("short memfd page write at %#x: wrote %d of %d", guestOffset, written, len(page))
	}
	if err := server.uffdOps.wake(uffd, faultPage, hostPageSize); err != nil {
		return fmt.Errorf("UFFDIO_WAKE page at %#x: %w", faultPage, err)
	}
	return nil
}

type uffdioRange struct {
	Start  uint64
	Length uint64
}

func wakeUFFDRange(fd int, start, length uint64) error {
	request := uffdioRange{Start: start, Length: length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uffdioWakeRequest, uintptr(unsafe.Pointer(&request)))
	if errno != 0 {
		return errno
	}
	return nil
}
