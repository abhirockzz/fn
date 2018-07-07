package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.opencensus.io/trace"

	"github.com/fnproject/cloudevent"
	"github.com/fnproject/fn/api/agent/drivers"
	"github.com/fnproject/fn/api/common"
	"github.com/fnproject/fn/api/id"
	"github.com/fnproject/fn/api/models"
	"github.com/sirupsen/logrus"
)

type Call interface {
	// Model will return the underlying models.Call configuration for this call.
	// TODO we could respond to async correctly from agent but layering, this
	// is only because the front end has different responses based on call type.
	// try to discourage use elsewhere until this gets pushed down more...
	Model() *models.Call

	// Start will be called before this call is executed, it may be used to
	// guarantee mutual exclusion, check docker permissions, update timestamps,
	// etc.
	// TODO Start and End can likely be unexported as they are only used in the agent,
	// and on a type which is constructed in a specific agent. meh.
	Start(ctx context.Context) error

	// End will be called immediately after attempting a call execution,
	// regardless of whether the execution failed or not. An error will be passed
	// to End, which if nil indicates a successful execution. Any error returned
	// from End will be returned as the error from Submit.
	End(ctx context.Context, err error) error
}

// Interceptor in GetCall
type CallOverrider func(*models.Call, map[string]string) (map[string]string, error)

// TODO build w/o closures... lazy
type CallOpt func(ctx context.Context, c *call) error

const (
	ceMimeType = "application/cloudevents+json"
)

// FromRequest initialises a call to a route from an HTTP request
// deprecate with routes
func FromRequest(app *models.App, route *models.Route, req *http.Request) CallOpt {
	return func(ctx context.Context, c *call) error {
		log := common.Logger(ctx)
		// Check whether this is a CloudEvent, if coming in via HTTP router (only way currently), then we'll look for a special header
		// Content-Type header: https://github.com/cloudevents/spec/blob/master/http-transport-binding.md#32-structured-content-mode
		// Expected Content-Type for a CloudEvent: application/cloudevents+json; charset=UTF-8
		contentType := req.Header.Get("Content-Type")
		t, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			// won't fail here, but log
			log.Debugf("Could not parse Content-Type header: %v", err)
		} else {
			if t == ceMimeType {
				c.IsCloudEvent = true
				route.Format = models.FormatCloudEvent
			}
		}

		if route.Format == "" {
			route.Format = models.FormatDefault
		}

		id := id.New().String()

		// TODO this relies on ordering of opts, but tests make sure it works, probably re-plumb/destroy headers
		// TODO async should probably supply an http.ResponseWriter that records the logs, to attach response headers to
		if rw, ok := c.w.(http.ResponseWriter); ok {
			rw.Header().Add("FN_CALL_ID", id)
			for k, vs := range route.Headers {
				for _, v := range vs {
					// pre-write in these headers to response
					rw.Header().Add(k, v)
				}
			}
		}

		// this ensures that there is an image, path, timeouts, memory, etc are valid.
		// NOTE: this means assign any changes above into route's fields
		err = route.Validate()
		if err != nil {
			return err
		}

		var syslogURL string
		if app.SyslogURL != nil {
			syslogURL = *app.SyslogURL
		}

		c.Call = &models.Call{
			ID:    id,
			Path:  route.Path,
			Image: route.Image,
			// Delay: 0,
			Type:        route.Type,
			Format:      route.Format,
			Priority:    new(int32), // TODO this is crucial, apparently
			Timeout:     route.Timeout,
			IdleTimeout: route.IdleTimeout,
			TmpFsSize:   route.TmpFsSize,
			Memory:      route.Memory,
			CPUs:        route.CPUs,
			Config:      buildConfig(app, route),
			Annotations: app.Annotations.MergeChange(route.Annotations),
			Headers:     req.Header,
			CreatedAt:   common.DateTime(time.Now()),
			URL:         reqURL(req),
			Method:      req.Method,
			AppID:       app.ID,
			SyslogURL:   syslogURL,
		}

		return setCallPayload(ctx, req.Body, c)
	}
}

// SPEC:

//cloudevent {
//eventType: "",
//...
//extensions: {
//fn: {
//id: "",
//name: "",
//image: "",
//...
//},
//trigger: {
//id: "",
//name: "",
//...
//},
//app: {
//id: "",
//name: "",
//}
//protocol: {
//type: "",
//extensions: {
//"": "",
//}
//}
//},
//}

func buildCloudEvent(req *http.Request) (*cloudevent.CloudEvent, error) {
	var ce cloudevent.CloudEvent
	// XXX(reed): ???
	ext := make(map[string]interface{}, 3)
	ext["app"] = new(models.App)
	ext["trigger"] = new(models.Trigger)
	ext["fn"] = new(models.Fn)
	ce.Extensions = ext
	err := ce.FromRequest(req)
	return &ce, err
}

// XXX(reed): for the split mode we need to support invoke that takes a fully built event, with
// the function/app/trigger unwound inside the event. we also need a way to build this state up,
// it's possible the two should interlope but maybe not. start without building squat here.
//
// thinking: we add the concrete event onto the call object to tote around and re-encode to the container,
// and a call is simply the extraction of information we need from the event object for the agent to use.
// we also need to plumb out the container responses all the way up preferably so that Submit returns an event?
//
// trigger only things?
// XXX(reed): shove headers into `protocol: { headers: { } }`
// XXX(reed): shove url into `protocol: `{ url: "" }` ? also eventURL
// XXX(reed): shove method into `protocol: `{ method: "" }` ? also eventURL
func FromEvent(event *cloudevent.CloudEvent) CallOpt {
	return func(ctx context.Context, c *call) error {
		ext, ok := event.Extensions.(map[string]interface{}) // XXX(reed): ?
		if !ok {
			return errors.New("cloud event extensions must be marshaled with known type")
		}

		// XXX(reed): prob need a map. ignore for a minute
		app := ext["app"].(*models.App)
		fn := ext["fn"].(*models.Fn)
		trigger := ext["fn"].(*models.Trigger)

		var syslogURL string
		if app.SyslogURL != nil {
			syslogURL = *app.SyslogURL
		}

		c.Call = &models.Call{
			// XXX(reed): these are the fields agent needs to run the thing, everything else
			// we can leave in cloud event format.
			// DO NOT MODIFY FIELDS DINGUS
			ID:          id.New().String(),
			Image:       fn.Image,
			Timeout:     fn.Timeout,
			IdleTimeout: fn.IdleTimeout,
			TmpFsSize:   0, // TODO clean up this
			Memory:      fn.Memory,
			CPUs:        0, // TODO clean up this
			SyslogURL:   syslogURL,
			// TODO - this wasn't really the intention here (that annotations would naturally cascade
			// but seems to be necessary for some runner behaviour
			// XXX(reed): we need annotations right?
			Annotations: app.Annotations.MergeChange(fn.Annotations).MergeChange(trigger.Annotations),
			// XXX(reed): some checksum / version for hotties (ugh)
			// XXX(reed): http handler should add eventURL ?

			// TODO DEPRECATE / NUKE
			Type:   "sync",
			Format: "cloudevent",
		}

		return nil
	}
}

// Sets up a call from an http trigger request
// TODO this should use FromEvent
func FromHTTPTriggerRequest(app *models.App, fn *models.Fn, trigger *models.Trigger, req *http.Request) CallOpt {
	return func(ctx context.Context, c *call) error {
		log := common.Logger(ctx)
		// Check whether this is a CloudEvent, if coming in via HTTP router (only way currently), then we'll look for a special header
		// Content-Type header: https://github.com/cloudevents/spec/blob/master/http-transport-binding.md#32-structured-content-mode
		// Expected Content-Type for a CloudEvent: application/cloudevents+json; charset=UTF-8
		contentType := req.Header.Get("Content-Type")
		t, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			// won't fail here, but log
			log.Debugf("Could not parse Content-Type header: %v", err)
		} else {
			if t == ceMimeType {
				c.IsCloudEvent = true
				fn.Format = models.FormatCloudEvent
			}
		}

		if fn.Format == "" {
			fn.Format = models.FormatDefault
		}

		id := id.New().String()

		// TODO this relies on ordering of opts, but tests make sure it works, probably re-plumb/destroy headers
		// TODO async should probably supply an http.ResponseWriter that records the logs, to attach response headers to
		if rw, ok := c.w.(http.ResponseWriter); ok {
			rw.Header().Add("FN_CALL_ID", id)
		}

		var syslogURL string
		if app.SyslogURL != nil {
			syslogURL = *app.SyslogURL
		}

		c.Call = &models.Call{
			ID:    id,
			Path:  trigger.Source,
			Image: fn.Image,
			// Delay: 0,
			Type:        "sync",
			Format:      fn.Format,
			Priority:    new(int32), // TODO this is crucial, apparently
			Timeout:     fn.Timeout,
			IdleTimeout: fn.IdleTimeout,
			TmpFsSize:   0, // TODO clean up this
			Memory:      fn.Memory,
			CPUs:        0, // TODO clean up this
			Config:      buildTriggerConfig(app, fn, trigger),
			// TODO - this wasn't really the intention here (that annotations would naturally cascade
			// but seems to be necessary for some runner behaviour
			Annotations: app.Annotations.MergeChange(fn.Annotations).MergeChange(trigger.Annotations),
			Headers:     req.Header,
			CreatedAt:   common.DateTime(time.Now()),
			URL:         reqURL(req),
			Method:      req.Method,
			AppID:       app.ID,
			FnID:        fn.ID,
			TriggerID:   trigger.ID,
			SyslogURL:   syslogURL,
		}

		return setCallPayload(ctx, req.Body, c)
	}
}

func buildConfig(app *models.App, route *models.Route) models.Config {
	conf := make(models.Config, 8+len(app.Config)+len(route.Config))
	for k, v := range app.Config {
		conf[k] = v
	}
	for k, v := range route.Config {
		conf[k] = v
	}

	conf["FN_FORMAT"] = route.Format
	conf["FN_APP_NAME"] = app.Name
	conf["FN_PATH"] = route.Path
	// TODO: might be a good idea to pass in: "FN_BASE_PATH" = fmt.Sprintf("/r/%s", appName) || "/" if using DNS entries per app
	conf["FN_MEMORY"] = fmt.Sprintf("%d", route.Memory)
	conf["FN_TYPE"] = route.Type
	conf["FN_TMPSIZE"] = fmt.Sprintf("%d", route.TmpFsSize)

	CPUs := route.CPUs.String()
	if CPUs != "" {
		conf["FN_CPUS"] = CPUs
	}
	return conf
}

func buildTriggerConfig(app *models.App, fn *models.Fn, trigger *models.Trigger) models.Config {
	conf := make(models.Config, 8+len(app.Config)+len(fn.Config))
	for k, v := range app.Config {
		conf[k] = v
	}
	for k, v := range fn.Config {
		conf[k] = v
	}

	conf["FN_FORMAT"] = fn.Format
	conf["FN_APP_NAME"] = app.Name
	conf["FN_PATH"] = trigger.Source
	// TODO: might be a good idea to pass in: "FN_BASE_PATH" = fmt.Sprintf("/r/%s", appName) || "/" if using DNS entries per app
	conf["FN_MEMORY"] = fmt.Sprintf("%d", fn.Memory)
	conf["FN_TYPE"] = "sync"
	conf["FN_FN_ID"] = fn.ID

	return conf
}

func reqURL(req *http.Request) string {
	if req.URL.Scheme == "" {
		if req.TLS == nil {
			req.URL.Scheme = "http"
		} else {
			req.URL.Scheme = "https"
		}
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	return req.URL.String()
}

// FromModel creates a call object from an existing stored call model object, reading the body from the stored call payload
func FromModel(mCall *models.Call) CallOpt {
	return func(ctx context.Context, c *call) error {
		c.Call = mCall
		return nil
	}
}

// FromModelAndInput creates a call object from an existing stored call model object , reading the body from a provided stream
func FromModelAndInput(mCall *models.Call, in io.ReadCloser) CallOpt {
	return func(ctx context.Context, c *call) error {
		c.Call = mCall
		return setCallPayload(ctx, in, c)
	}
}

// WithWriter sets the writier that the call uses to send its output message to
// TODO this should be required
func WithWriter(w io.Writer) CallOpt {
	return func(ctx context.Context, c *call) error {
		c.w = w
		return nil
	}
}

// WithExtensions adds internal attributes to the call that can be interpreted by extensions in the agent
// Pure runner can use this to pass an extension to the call
func WithExtensions(extensions map[string]string) CallOpt {
	return func(ctx context.Context, c *call) error {
		c.extensions = extensions
		return nil
	}
}

// GetCall builds a Call that can be used to submit jobs to the agent.
//
// TODO where to put this? async and sync both call this
func (a *agent) GetCall(ctx context.Context, opts ...CallOpt) (Call, error) {
	var c call

	for _, o := range opts {
		err := o(ctx, &c)
		if err != nil {
			return nil, err
		}
	}

	// TODO typed errors to test
	if c.Call == nil {
		return nil, errors.New("no model or request provided for call")
	}

	// If overrider is present, let's allow it to modify models.Call
	// and call extensions
	if a.callOverrider != nil {
		ext, err := a.callOverrider(c.Call, c.extensions)
		if err != nil {
			return nil, err
		}
		c.extensions = ext
	}

	mem := c.Memory + uint64(c.TmpFsSize)
	if !a.resources.IsResourcePossible(mem, uint64(c.CPUs), c.Type == models.TypeAsync) {
		// if we're not going to be able to run this call on this machine, bail here.
		return nil, models.ErrCallTimeoutServerBusy
	}

	setupCtx(&c)

	c.handler = a.da
	c.ct = a
	c.stderr = setupLogger(ctx, a.cfg.MaxLogSize, c.Call)
	if c.w == nil {
		// send STDOUT to logs if no writer given (async...)
		// TODO we could/should probably make this explicit to GetCall, ala 'WithLogger', but it's dupe code (who cares?)
		c.w = c.stderr
	}

	return &c, nil
}

// setCallPayload sets the payload on a call, respecting the context
func setCallPayload(ctx context.Context, input io.Reader, c *call) error {
	// WARNING: we need to handle IO in a separate go-routine below
	// to be able to detect a ctx timeout. When we timeout, we
	// let gin/http-server to unblock the go-routine below.
	errApp := make(chan error, 1)
	go func() {
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		_, err := buf.ReadFrom(input)
		if err != nil && err != io.EOF {
			errApp <- err
			return
		}

		c.Payload = buf.String()
		bufPool.Put(buf)
		close(errApp)
	}()

	select {
	case err := <-errApp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type call struct {
	*models.Call

	// IsCloudEvent flag whether this was ingested as a cloud event. This may become the default or only way.
	IsCloudEvent bool `json:"is_cloud_event"`

	handler        CallHandler
	w              io.Writer
	stderr         io.ReadWriteCloser
	ct             callTrigger
	slots          *slotQueue
	requestState   RequestState
	containerState ContainerState
	slotHashId     string
	isLB           bool

	// LB & Pure Runner Extra Config
	extensions map[string]string
}

// SlotHashId returns a string identity for this call that can be used to uniquely place the call in a given container
// This should correspond to a unique identity (including data changes) of the underlying function
func (c *call) SlotHashId() string {
	return c.slotHashId
}

func (c *call) Extensions() map[string]string {
	return c.extensions
}

func (c *call) RequestBody() io.ReadCloser {
	return ioutil.NopCloser(strings.NewReader(c.Payload))
}

func (c *call) ResponseWriter() http.ResponseWriter {
	return c.w.(http.ResponseWriter)
}

func (c *call) StdErr() io.ReadWriteCloser {
	return c.stderr
}

func (c *call) Model() *models.Call { return c.Call }

func (c *call) Start(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "agent_call_start")
	defer span.End()

	// Check context timeouts, errors
	if ctx.Err() != nil {
		return ctx.Err()
	}

	c.StartedAt = common.DateTime(time.Now())
	c.Status = "running"

	if !c.isLB {
		if rw, ok := c.w.(http.ResponseWriter); ok { // TODO need to figure out better way to wire response headers in
			rw.Header().Set("XXX-FXLB-WAIT", time.Time(c.StartedAt).Sub(time.Time(c.CreatedAt)).String())
		}
	}

	if c.Type == models.TypeAsync {
		// XXX (reed): make sure MQ reservation is lengthy. to skirt MQ semantics,
		// we could add a new message to MQ w/ delay of call.Timeout and delete the
		// old one (in that order), after marking the call as running in the db
		// (see below)

		// XXX (reed): should we store the updated started_at + status? we could
		// use this so that if we pick up a call from mq and find its status is
		// running to avoid running the call twice and potentially mark it as
		// errored (built in long running task detector, so to speak...)

		err := c.handler.Start(ctx, c.Model())
		if err != nil {
			return err // let another thread try this
		}
	}

	err := c.ct.fireBeforeCall(ctx, c.Model())
	if err != nil {
		return fmt.Errorf("BeforeCall: %v", err)
	}

	return nil
}

func (c *call) End(ctx context.Context, errIn error) error {
	ctx, span := trace.StartSpan(ctx, "agent_call_end")
	defer span.End()

	c.CompletedAt = common.DateTime(time.Now())

	switch errIn {
	case nil:
		c.Status = "success"
	case context.DeadlineExceeded:
		c.Status = "timeout"
	default:
		c.Status = "error"
		c.Error = errIn.Error()
	}

	// ensure stats histogram is reasonably bounded
	c.Call.Stats = drivers.Decimate(240, c.Call.Stats)

	if err := c.handler.Finish(ctx, c.Model(), c.stderr, c.Type == models.TypeAsync); err != nil {
		common.Logger(ctx).WithError(err).Error("error finalizing call on datastore/mq")
		// note: Not returning err here since the job could have already finished successfully.
	}

	// NOTE call this after InsertLog or the buffer will get reset
	c.stderr.Close()

	if err := c.ct.fireAfterCall(ctx, c.Model()); err != nil {
		return fmt.Errorf("AfterCall: %v", err)
	}

	return errIn // original error, important for use in sync call returns
}
