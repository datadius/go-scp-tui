/* Copyright (c) 2024 Bram Vandenbogaerde And Contributors
 * You may use, distribute or modify this code under the
 * terms of the Mozilla Public License 2.0, which is distributed
 * along with the source code.
 */

package scp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/crypto/ssh"
)

// Callback for freeing managed resources
type ICloseHandler interface {
	Close()
}

// Close handler equivalent to a no-op. Used by default
// when no resources have to be cleaned.
type EmptyHandler struct{}

func (EmptyHandler) Close() {}

// Close handler to close an SSH client
type CloseSSHCLient struct {
	// Reference to the used SSH client
	sshClient *ssh.Client
}

func (scp CloseSSHCLient) Close() {
	scp.sshClient.Close()
}

type PassThru func(r io.Reader, total int64) io.Reader

type Client struct {
	// Host the host to connect to.
	Host string

	// ClientConfig the client config to use.
	ClientConfig *ssh.ClientConfig

	// Keep the ssh client around for generating new sessions
	sshClient *ssh.Client

	// Timeout the maximal amount of time to wait for a file transfer to complete.
	// Deprecated: use context.Context for each function instead.
	Timeout time.Duration

	// RemoteBinary the absolute path to the remote SCP binary.
	RemoteBinary string

	// Handler called when calling `Close` to clean up any remaining
	// resources managed by `Client`.
	closeHandler ICloseHandler
}

// Connect connects to the remote SSH server, returns error if it couldn't establish a session to the SSH server.
func (a *Client) Connect() error {
	client, err := ssh.Dial("tcp", a.Host, a.ClientConfig)
	if err != nil {
		return err
	}

	a.sshClient = client
	a.closeHandler = CloseSSHCLient{sshClient: client}
	return nil
}

// Returns the underlying SSH client, this should be used carefully as
// it will be closed by `client.Close`.
func (a *Client) SSHClient() *ssh.Client {
	return a.sshClient
}

// CopyFromFile copies the contents of an os.File to a remote location, it will get the length of the file by looking it up from the filesystem.
func (a *Client) CopyFromFile(
	ctx context.Context,
	file os.File,
	remotePath string,
	permissions string,
) error {
	return a.CopyFromFilePassThru(ctx, file, remotePath, permissions, nil)
}

// CopyFromFilePassThru copies the contents of an os.File to a remote location, it will get the length of the file by looking it up from the filesystem.
// Access copied bytes by providing a PassThru reader factory.
func (a *Client) CopyFromFilePassThru(
	ctx context.Context,
	file os.File,
	remotePath string,
	permissions string,
	passThru PassThru,
) error {
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	return a.CopyPassThru(ctx, &file, remotePath, permissions, stat.Size(), passThru)
}

// CopyFile copies the contents of an io.Reader to a remote location, the length is determined by reading the io.Reader until EOF
// if the file length in know in advance please use "Copy" instead.
func (a *Client) CopyFile(
	ctx context.Context,
	fileReader io.Reader,
	remotePath string,
	permissions string,
) error {
	return a.CopyFilePassThru(ctx, fileReader, remotePath, permissions, nil)
}

// CopyFilePassThru copies the contents of an io.Reader to a remote location, the length is determined by reading the io.Reader until EOF
// if the file length in know in advance please use "Copy" instead.
// Access copied bytes by providing a PassThru reader factory.
func (a *Client) CopyFilePassThru(
	ctx context.Context,
	fileReader io.Reader,
	remotePath string,
	permissions string,
	passThru PassThru,
) error {
	contentsBytes, err := io.ReadAll(fileReader)
	if err != nil {
		return fmt.Errorf("failed to read all data from reader: %w", err)
	}
	bytesReader := bytes.NewReader(contentsBytes)

	return a.CopyPassThru(
		ctx,
		bytesReader,
		remotePath,
		permissions,
		int64(len(contentsBytes)),
		passThru,
	)
}

// wait waits for the waitgroup for the specified max timeout.
// Returns true if waiting timed out.
func wait(wg *sync.WaitGroup, ctx context.Context) error {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()

	select {
	case <-c:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkResponse checks the response it reads from the remote, and will return a single error in case
// of failure.
func checkResponse(r io.Reader) error {
	response, err := ParseResponse(r)
	if err != nil {
		return err
	}

	if response.IsFailure() {
		return errors.New(response.GetMessage())
	}

	return nil

}

// Copy copies the contents of an io.Reader to a remote location.
func (a *Client) Copy(
	ctx context.Context,
	r io.Reader,
	remotePath string,
	permissions string,
	size int64,
) error {
	return a.CopyPassThru(ctx, r, remotePath, permissions, size, nil)
}

// CopyPassThru copies the contents of an io.Reader to a remote location.
// Access copied bytes by providing a PassThru reader factory
func (a *Client) CopyPassThru(
	ctx context.Context,
	r io.Reader,
	remotePath string,
	permissions string,
	size int64,
	passThru PassThru,
) error {
	session, err := a.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("Error creating ssh session in copy to remote: %v", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	w, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer w.Close()

	if passThru != nil {
		r = passThru(r, size)
	}

	filename := path.Base(remotePath)

	// Start the command first and get confirmation that it has been started
	// before sending anything through the pipes.
	err = session.Start(fmt.Sprintf("%s -qt %q", a.RemoteBinary, remotePath))
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	errCh := make(chan error, 2)

	// SCP protocol and file sending
	go func() {
		defer wg.Done()
		defer w.Close()

		_, err = fmt.Fprintln(w, "C"+permissions, size, filename)
		if err != nil {
			errCh <- err
			return
		}

		if err = checkResponse(stdout); err != nil {
			errCh <- err
			return
		}

		_, err = io.Copy(w, r)
		if err != nil {
			errCh <- err
			return
		}

		_, err = fmt.Fprint(w, "\x00")
		if err != nil {
			errCh <- err
			return
		}

		if err = checkResponse(stdout); err != nil {
			errCh <- err
			return
		}
	}()

	// Wait for the process to exit
	go func() {
		defer wg.Done()
		err := session.Wait()
		if err != nil {
			errCh <- err
			return
		}
	}()

	// If there is a timeout, stop the transfer if it has been exceeded
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	// Wait for one of the conditions (error/timeout/completion) to occur
	if err := wait(&wg, ctx); err != nil {
		return err
	}

	close(errCh)

	// Collect any errors from the error channel
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return nil
}

// CopyFromRemote copies a file from the remote to the local file given by the `file`
// parameter. Use `CopyFromRemotePassThru` if a more generic writer
// is desired instead of writing directly to a file on the file system.?
func (a *Client) CopyFromRemote(ctx context.Context, file *os.File, remotePath string) error {
	return a.CopyFromRemotePassThru(ctx, file, remotePath, nil)
}

// CopyFromRemotePassThru copies a file from the remote to the given writer. The passThru parameter can be used
// to keep track of progress and how many bytes that were download from the remote.
// `passThru` can be set to nil to disable this behaviour.
func (a *Client) CopyFromRemotePassThru(
	ctx context.Context,
	w io.Writer,
	remotePath string,
	passThru PassThru,
) error {
	session, err := a.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("Error creating ssh session in copy from remote: %v", err)
	}
	defer session.Close()

	wg := sync.WaitGroup{}
	errCh := make(chan error, 4)

	wg.Add(1)
	go func() {
		var err error

		defer func() {
			// NOTE: this might send an already sent error another time, but since we only receive opne, this is fine. On the "happy-path" of this function, the error will be `nil` therefore completing the "err<-errCh" at the bottom of the function.
			errCh <- err
			// We must unblock the go routine first as we block on reading the channel later
			wg.Done()

		}()

		r, err := session.StdoutPipe()
		if err != nil {
			errCh <- err
			return
		}

		in, err := session.StdinPipe()
		if err != nil {
			errCh <- err
			return
		}
		defer in.Close()

		err = session.Start(fmt.Sprintf("%s -f %q", a.RemoteBinary, remotePath))
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		res, err := ParseResponse(r)
		if err != nil {
			errCh <- err
			return
		}
		if res.IsFailure() {
			errCh <- errors.New(res.GetMessage())
			return
		}

		infos, err := res.ParseFileInfos()
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		if passThru != nil {
			r = passThru(r, infos.Size)
		}

		_, err = CopyN(w, r, infos.Size)
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		err = session.Wait()
		if err != nil {
			errCh <- err
			return
		}
	}()

	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	if err := wait(&wg, ctx); err != nil {
		return err
	}
	finalErr := <-errCh
	close(errCh)
	return finalErr
}

var p *tea.Program

type progressWriter struct {
	total      int64
	downloaded int
	file       io.Writer
	reader     io.Reader
	onProgress func(float64)
}

func (pw *progressWriter) Start() error {
	_, err := CopyN(pw.file, io.TeeReader(pw.reader, pw), pw.total)
	//var total int64
	//total = 0
	//for total < pw.total {
	//	n, err := CopyN(pw.file, pw.reader, pw.total)
	//	pw.downloaded += n
	//	if pw.total > 0 && pw.onProgress != nil {
	//		pw.onProgress(float64(pw.downloaded) / float64(pw.total))
	//	}
	//	if err != nil {
	//		fmt.Println(err)
	//		p.Send(progressErrMsg{err})
	//	}
	//	total += n
	//}

	//_, err := CopyN(pw.file, pw.reader, pw.total)
	if err != nil {
		p.Send(progressErrMsg{err})
	}
	return err
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.downloaded += len(p)
	if pw.total > 0 && pw.onProgress != nil {
		pw.onProgress(float64(pw.downloaded) / float64(pw.total))
	}
	return len(p), nil
}

func (a *Client) CopyFromRemoteProgressPassThru(
	ctx context.Context,
	w io.Writer,
	remotePath string,
	passThru PassThru,
) error {
	session, err := a.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("Error creating ssh session in copy from remote: %v", err)
	}
	defer session.Close()

	wg := sync.WaitGroup{}
	errCh := make(chan error, 4)

	wg.Add(1)
	go func() {
		var err error

		defer func() {
			// NOTE: this might send an already sent error another time, but since we only receive opne, this is fine. On the "happy-path" of this function, the error will be `nil` therefore completing the "err<-errCh" at the bottom of the function.
			errCh <- err
			// We must unblock the go routine first as we block on reading the channel later
			wg.Done()

		}()

		r, err := session.StdoutPipe()
		if err != nil {
			errCh <- err
			return
		}

		in, err := session.StdinPipe()
		if err != nil {
			errCh <- err
			return
		}
		defer in.Close()

		err = session.Start(fmt.Sprintf("%s -f %q", a.RemoteBinary, remotePath))
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		res, err := ParseResponse(r)
		if err != nil {
			errCh <- err
			return
		}
		if res.IsFailure() {
			errCh <- errors.New(res.GetMessage())
			return
		}

		infos, err := res.ParseFileInfos()
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		if passThru != nil {
			r = passThru(r, infos.Size)
		}

		pw := &progressWriter{
			total:  infos.Size,
			file:   w,
			reader: r,
			onProgress: func(ratio float64) {
				p.Send(progressMsg(ratio))
			},
		}

		m := model{
			pw:       pw,
			progress: progress.New(progress.WithDefaultGradient()),
		}

		p = tea.NewProgram(m)

		go pw.Start()

		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		if _, err := p.Run(); err != nil {
			fmt.Println("Error running progress: ", err)
			os.Exit(1)
		}

		err = session.Wait()
		if err != nil {
			errCh <- err
			return
		}

	}()

	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	if err := wait(&wg, ctx); err != nil {
		return err
	}
	finalErr := <-errCh
	close(errCh)
	return finalErr
}

func (a *Client) CopyFromRemotePreserveProgressPassThru(
	ctx context.Context,
	w io.Writer,
	remotePath string,
	passThru PassThru,
) error {
	session, err := a.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("Error creating ssh session in copy from remote: %v", err)
	}
	defer session.Close()

	wg := sync.WaitGroup{}
	errCh := make(chan error, 4)

	wg.Add(1)
	go func() {
		var err error

		defer func() {
			// NOTE: this might send an already sent error another time, but since we only receive opne, this is fine. On the "happy-path" of this function, the error will be `nil` therefore completing the "err<-errCh" at the bottom of the function.
			errCh <- err
			// We must unblock the go routine first as we block on reading the channel later
			wg.Done()

		}()

		r, err := session.StdoutPipe()
		if err != nil {
			errCh <- err
			return
		}

		in, err := session.StdinPipe()
		if err != nil {
			errCh <- err
			return
		}
		defer in.Close()

		err = session.Start(fmt.Sprintf("%s -f -p %q", a.RemoteBinary, remotePath))
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		res, err := ParseResponse(r)
		if err != nil {
			errCh <- err
			return
		}
		if res.IsFailure() && res.NoStandardProtocolType() {
			errCh <- errors.New(res.GetMessage())
			return
		}

		timeInfo, err := res.ParseFileTime()
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		res, err = ParseResponse(r)
		if err != nil {
			errCh <- err
			return
		}
		if res.IsFailure() && res.NoStandardProtocolType() {
			errCh <- errors.New(res.GetMessage())
			return
		}

		infos, err := res.ParseFileInfos()
		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		infos.Update(timeInfo)

		if passThru != nil {
			r = passThru(r, infos.Size)
		}

		pw := &progressWriter{
			total:  infos.Size,
			file:   w,
			reader: r,
			onProgress: func(ratio float64) {
				p.Send(progressMsg(ratio))
			},
		}

		m := model{
			pw:       pw,
			progress: progress.New(progress.WithDefaultGradient()),
		}

		p = tea.NewProgram(m)

		go pw.Start()

		if err != nil {
			errCh <- err
			return
		}

		err = Ack(in)
		if err != nil {
			errCh <- err
			return
		}

		if _, err := p.Run(); err != nil {
			fmt.Println("Error running progress: ", err)
			os.Exit(1)
		}

		err = session.Wait()
		if err != nil {
			errCh <- err
			return
		}

	}()

	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	if err := wait(&wg, ctx); err != nil {
		return err
	}
	finalErr := <-errCh
	close(errCh)
	return finalErr
}

func (a *Client) Close() {
	a.closeHandler.Close()
}
