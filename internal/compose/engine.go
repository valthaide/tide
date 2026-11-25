package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const engineAPIVersion = "v1.44"

// engineClient reads container and image state from the Docker Engine API.
type engineClient struct {
	base string
	http *http.Client
}

var sharedEngineState struct {
	sync.Mutex
	host   string
	client *engineClient
}

// sharedEngine returns the one process-wide engine client. Every caller
// shares its http.Transport, so idle connections to docker.sock are reused
// instead of accumulating per call. The client is rebuilt only when
// DOCKER_HOST changes; the previous client's idle connections are closed.
func sharedEngine() (*engineClient, error) {
	raw := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if raw == "" {
		raw = "unix:///var/run/docker.sock"
	}
	sharedEngineState.Lock()
	defer sharedEngineState.Unlock()
	if sharedEngineState.client != nil && sharedEngineState.host == raw {
		return sharedEngineState.client, nil
	}
	if sharedEngineState.client != nil {
		sharedEngineState.client.closeIdle()
	}
	client, err := newEngineClient()
	if err != nil {
		return nil, err
	}
	sharedEngineState.host = raw
	sharedEngineState.client = client
	return client, nil
}

// closeIdle releases idle keep-alive connections held by the client's
// transport. Callers that build a temporary client (tests) must call it
// when done.
func (c *engineClient) closeIdle() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

type engineContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
}

func newEngineClient() (*engineClient, error) {
	raw := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if raw == "" {
		raw = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = "/var/run/docker.sock"
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		}
		return &engineClient{base: "http://docker", http: &http.Client{Transport: transport}}, nil
	case "tcp":
		return &engineClient{base: "http://" + u.Host, http: &http.Client{}}, nil
	case "http", "https":
		return &engineClient{base: strings.TrimRight(raw, "/"), http: &http.Client{}}, nil
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %s", u.Scheme)
	}
}

func (c *engineClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("docker engine %s: %s: %s", path, resp.Status, msg)
	}
	return body, nil
}

func (c *engineClient) listComposeContainers(ctx context.Context) ([]engineContainer, error) {
	q := url.Values{}
	q.Set("all", "1")
	q.Set("filters", `{"label":["com.docker.compose.project"]}`)
	body, err := c.get(ctx, "/"+engineAPIVersion+"/containers/json?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var listed []engineContainer
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, fmt.Errorf("docker engine container list: %w", err)
	}
	return listed, nil
}

func (c *engineClient) inspectContainer(ctx context.Context, id string) ([]byte, error) {
	return c.get(ctx, "/"+engineAPIVersion+"/containers/"+url.PathEscape(id)+"/json")
}

func (c *engineClient) inspectImage(ctx context.Context, id string) ([]byte, error) {
	return c.get(ctx, "/"+engineAPIVersion+"/images/"+url.PathEscape(id)+"/json")
}

func (c *engineClient) inspectNetwork(ctx context.Context, name string) ([]byte, error) {
	return c.get(ctx, "/"+engineAPIVersion+"/networks/"+url.PathEscape(name))
}

func (c *engineClient) listProjectContainers(ctx context.Context, project string) ([]engineContainer, error) {
	q := url.Values{}
	q.Set("all", "1")
	filterJSON, _ := json.Marshal(map[string][]string{
		"label": {"com.docker.compose.project=" + project},
	})
	q.Set("filters", string(filterJSON))
	body, err := c.get(ctx, "/"+engineAPIVersion+"/containers/json?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var listed []engineContainer
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, fmt.Errorf("docker engine container list: %w", err)
	}
	return listed, nil
}

func (e *ComposeExecutor) observeContainers(ctx context.Context) ([]containerObs, error) {
	return e.observeProject(ctx, "")
}

func (e *ComposeExecutor) observeProject(ctx context.Context, project string) ([]containerObs, error) {
	client, err := sharedEngine()
	if err != nil {
		return nil, err
	}
	return e.readProjectObservations(ctx, client, project)
}

func (e *ComposeExecutor) readEngineObservations(ctx context.Context, client *engineClient) ([]containerObs, error) {
	return e.readProjectObservations(ctx, client, "")
}

func (e *ComposeExecutor) readProjectObservations(ctx context.Context, client *engineClient, project string) ([]containerObs, error) {
	var listed []engineContainer
	var err error
	if project = strings.TrimSpace(project); project != "" {
		listed, err = client.listProjectContainers(ctx, project)
	} else {
		listed, err = client.listComposeContainers(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("docker engine container list failed: %w", err)
	}
	var obs []containerObs
	for _, item := range listed {
		project := strings.TrimSpace(item.Labels["com.docker.compose.project"])
		if project == "" {
			continue
		}
		oneoff := strings.TrimSpace(item.Labels["com.docker.compose.oneoff"])
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(strings.TrimSpace(item.Names[0]), "/")
		}
		c := containerObs{
			ID:       strings.TrimSpace(item.ID),
			Project:  project,
			Service:  strings.TrimSpace(item.Labels["com.docker.compose.service"]),
			Name:     name,
			OneOff:   strings.EqualFold(oneoff, "true") || oneoff == "1",
			Image:    strings.TrimSpace(item.Image),
			ImageID:  strings.TrimSpace(item.ImageID),
			PSStatus: strings.TrimSpace(item.Status),
			State:    strings.TrimSpace(item.State),
		}
		c.Running = c.State == "running" || dockerStatusIsRunning(c.PSStatus)
		obs = append(obs, c)
	}
	if len(obs) == 0 {
		return obs, nil
	}

	ids := make([]string, len(obs))
	for i := range obs {
		ids[i] = obs[i].ID
	}
	bodies := fetchParallel(ctx, ids, func(ctx context.Context, id string) ([]byte, error) {
		body, err := client.inspectContainer(ctx, id)
		if err != nil && e != nil && e.Logger != nil {
			e.Logger.Warn("docker engine inspect failed; restart and oneshot details unavailable", "id", id, "error", err)
		}
		return body, err
	})
	var inspected []json.RawMessage
	for _, body := range bodies {
		if body != nil {
			inspected = append(inspected, json.RawMessage(body))
		}
	}
	if len(inspected) > 0 {
		raw, err := json.Marshal(inspected)
		if err != nil {
			return nil, err
		}
		if err := applyContainerInspectJSON(obs, raw); err != nil && e != nil && e.Logger != nil {
			e.Logger.Warn("docker engine inspect json rejected", "error", err)
		}
	}

	refs := imageRefs(obs)
	imageBodies := fetchParallel(ctx, refs, func(ctx context.Context, ref string) ([]byte, error) {
		body, err := e.cachedImageInspect(ctx, client, ref)
		if err != nil && e != nil && e.Logger != nil {
			e.Logger.Warn("docker engine image inspect failed; digests unavailable", "image", ref, "error", err)
		}
		return body, err
	})
	var images []json.RawMessage
	for _, body := range imageBodies {
		if body != nil {
			images = append(images, json.RawMessage(body))
		}
	}
	if len(images) > 0 {
		raw, err := json.Marshal(images)
		if err != nil {
			return nil, err
		}
		if err := applyImageInspectJSON(obs, raw); err != nil && e != nil && e.Logger != nil {
			e.Logger.Warn("docker engine image inspect json rejected", "error", err)
		}
	}
	return obs, nil
}

const engineParallel = 8

const imageInspectTTL = 10 * time.Minute

type cachedImage struct {
	body []byte
	at   time.Time
}

func fetchParallel(ctx context.Context, keys []string, fetch func(context.Context, string) ([]byte, error)) [][]byte {
	out := make([][]byte, len(keys))
	sem := make(chan struct{}, engineParallel)
	var wg sync.WaitGroup
	for i, key := range keys {
		if key == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			if body, err := fetch(ctx, key); err == nil {
				out[i] = body
			}
		}(i, key)
	}
	wg.Wait()
	return out
}

func (e *ComposeExecutor) cachedImageInspect(ctx context.Context, client *engineClient, ref string) ([]byte, error) {
	cacheable := e != nil && strings.HasPrefix(ref, "sha256:")
	now := time.Now()
	if cacheable {
		e.imageMu.Lock()
		hit, ok := e.imageCache[ref]
		e.imageMu.Unlock()
		if ok && now.Sub(hit.at) < imageInspectTTL {
			return hit.body, nil
		}
	}
	body, err := client.inspectImage(ctx, ref)
	if err != nil || !cacheable {
		return body, err
	}
	e.imageMu.Lock()
	if e.imageCache == nil {
		e.imageCache = map[string]cachedImage{}
	}
	for key, item := range e.imageCache {
		if now.Sub(item.at) >= imageInspectTTL {
			delete(e.imageCache, key)
		}
	}
	e.imageCache[ref] = cachedImage{body: body, at: now}
	e.imageMu.Unlock()
	return body, nil
}
