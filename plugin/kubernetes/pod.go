package kubernetes

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/pkg/errors"
	"github.com/puppetlabs/wash/journal"
	"github.com/puppetlabs/wash/plugin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8exec "k8s.io/client-go/util/exec"
)

type pod struct {
	plugin.EntryBase
	client *k8s.Clientset
	config *rest.Config
	ns     string
}

func newPod(client *k8s.Clientset, config *rest.Config, ns string, p *corev1.Pod) *pod {
	pd := &pod{
		EntryBase: plugin.NewEntry(p.Name),
		client:    client,
		config:    config,
		ns:        ns,
	}
	pd.Ctime = p.CreationTimestamp.Time

	return pd
}

func (p *pod) Metadata(ctx context.Context) (plugin.MetadataMap, error) {
	pd, err := p.client.CoreV1().Pods(p.ns).Get(p.Name(), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	journal.Record(ctx, "Metadata for pod %v: %+v", p.Name(), pd)
	return plugin.ToMetadata(pd), nil
}

func (p *pod) Attr(ctx context.Context) (plugin.Attributes, error) {
	return plugin.Attributes{
		Ctime: p.Ctime,
		Mtime: time.Now(),
		Atime: p.Ctime,
		Size:  plugin.SizeUnknown,
	}, nil
}

func (p *pod) Open(ctx context.Context) (plugin.SizedReader, error) {
	req := p.client.CoreV1().Pods(p.ns).GetLogs(p.Name(), &corev1.PodLogOptions{})
	rdr, err := req.Stream()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	var n int64
	if n, err = buf.ReadFrom(rdr); err != nil {
		return nil, err
	}
	journal.Record(ctx, "Read %v bytes of %v log", n, p.Name())
	return bytes.NewReader(buf.Bytes()), nil
}

func (p *pod) Stream(ctx context.Context) (io.Reader, error) {
	var tailLines int64 = 10
	req := p.client.CoreV1().Pods(p.ns).GetLogs(p.Name(), &corev1.PodLogOptions{Follow: true, TailLines: &tailLines})
	return req.Stream()
}

func (p *pod) Exec(ctx context.Context, cmd string, args []string, opts plugin.ExecOptions) (plugin.ExecResult, error) {
	execRequest := p.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(p.Name()).
		Namespace(p.ns).
		SubResource("exec").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("command", cmd)

	for _, arg := range args {
		execRequest = execRequest.Param("command", arg)
	}

	if opts.Stdin != nil {
		execRequest = execRequest.Param("stdin", "true")
	}

	execResult := plugin.ExecResult{}

	executor, err := remotecommand.NewSPDYExecutor(p.config, "POST", execRequest.URL())
	if err != nil {
		return execResult, errors.Wrap(err, "kubernetes.pod.Exec request")
	}

	outputCh, stdout, stderr := plugin.CreateExecOutputStreams(ctx)
	exitcode := 0
	go func() {
		streamOpts := remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr, Stdin: opts.Stdin}
		err = executor.Stream(streamOpts)
		journal.Record(ctx, "Exec on %v complete: %v", p.Name(), err)
		if exerr, ok := err.(k8exec.ExitError); ok {
			exitcode = exerr.ExitStatus()
			err = nil
		}

		stdout.CloseWithError(err)
		stderr.CloseWithError(err)
	}()

	execResult.OutputCh = outputCh
	execResult.ExitCodeCB = func() (int, error) {
		return exitcode, nil
	}

	return execResult, nil
}
