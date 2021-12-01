package cloudmonitoring

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/grafana/grafana-google-sdk-go/pkg/utils"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
)

// nameExp matches the part after the last '/' symbol
var nameExp = regexp.MustCompile(`([^\/]*)\/*$`)

const resourceManagerPath = "/v1/projects"

type processResponse func(body []byte, results []json.RawMessage) ([]json.RawMessage, string, error)

func (s *Service) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/gceDefaultProject", getGCEDefaultProject)

	mux.HandleFunc("/metricDescriptors/", s.resourceHandler(cloudMonitor, processMetricDescriptors))
	mux.HandleFunc("/services/", s.resourceHandler(cloudMonitor, processServices))
	mux.HandleFunc("/slo-services/", s.resourceHandler(cloudMonitor, processSLOs))
	mux.HandleFunc("/projects", s.resourceHandler(resourceManager, processProjects))
}

func getGCEDefaultProject(rw http.ResponseWriter, req *http.Request) {
	project, err := utils.GCEDefaultProject(req.Context())
	if err != nil {
		writeResponse(rw, http.StatusBadRequest, fmt.Sprintf("unexpected error %v", err))
		return
	}
	writeResponse(rw, http.StatusOK, project)
}

func (s *Service) resourceHandler(subDataSource string, responseFn processResponse) func(rw http.ResponseWriter, req *http.Request) {
	return func(rw http.ResponseWriter, req *http.Request) {
		client, code, err := s.setRequestVariables(req, subDataSource)
		if err != nil {
			writeResponse(rw, code, fmt.Sprintf("unexpected error %v", err))
			return
		}
		doRequest(rw, req, client, responseFn)
	}
}

func doRequest(rw http.ResponseWriter, req *http.Request, cli *http.Client, responseFn processResponse) http.ResponseWriter {
	if responseFn == nil {
		writeResponse(rw, http.StatusInternalServerError, "responseFn should not be nil")
		return rw
	}

	responses, headers, encoding, code, err := loopRequest(req, cli, responseFn)
	if err != nil {
		writeResponse(rw, code, fmt.Sprintf("unexpected error %v", err))
		return rw
	}

	body, errcode, err := buildResponse(responses, encoding)
	if err != nil {
		writeResponse(rw, errcode, fmt.Sprintf("error formatting responose %v", err))
		return rw
	}
	writeResponseBytes(rw, code, body)

	for k, v := range headers {
		rw.Header().Set(k, v[0])
		for _, v := range v[1:] {
			rw.Header().Add(k, v)
		}
	}
	return rw
}

func processMetricDescriptors(body []byte, results []json.RawMessage) ([]json.RawMessage, string, error) {
	resp := metricDescriptorResponse{}
	err := json.Unmarshal(body, &resp)
	if err != nil {
		return nil, "", err
	}

	for i := range resp.Descriptors {
		resp.Descriptors[i].Service = strings.SplitN(resp.Descriptors[i].Type, "/", 2)[0]
		resp.Descriptors[i].ServiceShortName = strings.SplitN(resp.Descriptors[i].Service, ".", 2)[0]
		if resp.Descriptors[i].DisplayName == "" {
			resp.Descriptors[i].DisplayName = resp.Descriptors[i].Type
		}
		descriptor, err := json.Marshal(resp.Descriptors[i])
		if err != nil {
			return nil, "", err
		}
		results = append(results, descriptor)
	}
	return results, resp.Token, nil
}

func processServices(body []byte, results []json.RawMessage) ([]json.RawMessage, string, error) {
	resp := serviceResponse{}
	err := json.Unmarshal(body, &resp)
	if err != nil {
		return nil, "", err
	}

	for _, service := range resp.Services {
		name := nameExp.FindString(service.Name)
		if name == "" {
			return nil, "", fmt.Errorf("unexpected service name: %v", service.Name)
		}
		label := service.DisplayName
		if label == "" {
			label = name
		}
		marshaledValue, err := json.Marshal(selectableValue{
			Value: name,
			Label: label,
		})
		if err != nil {
			return nil, "", err
		}
		results = append(results, marshaledValue)
	}
	return results, resp.Token, nil
}

func processSLOs(body []byte, results []json.RawMessage) ([]json.RawMessage, string, error) {
	resp := sloResponse{}
	err := json.Unmarshal(body, &resp)
	if err != nil {
		return nil, "", err
	}

	for _, slo := range resp.SLOs {
		name := nameExp.FindString(slo.Name)
		if name == "" {
			return nil, "", fmt.Errorf("unexpected service name: %v", slo.Name)
		}
		marshaledValue, err := json.Marshal(selectableValue{
			Value: name,
			Label: slo.DisplayName,
			Goal:  slo.Goal,
		})
		if err != nil {
			return nil, "", err
		}
		results = append(results, marshaledValue)
	}
	return results, resp.Token, nil
}

func processProjects(body []byte, results []json.RawMessage) ([]json.RawMessage, string, error) {
	resp := projectResponse{}
	err := json.Unmarshal(body, &resp)
	if err != nil {
		return nil, "", err
	}

	for _, project := range resp.Projects {
		marshaledValue, err := json.Marshal(selectableValue{
			Value: project.ProjectID,
			Label: project.Name,
		})
		if err != nil {
			return nil, "", err
		}
		results = append(results, marshaledValue)
	}
	return results, resp.Token, nil
}

func decode(encoding string, original io.ReadCloser) ([]byte, int, error) {
	var reader io.Reader
	var err error
	switch encoding {
	case "gzip":
		reader, err = gzip.NewReader(original)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		defer func() {
			if err := reader.(io.ReadCloser).Close(); err != nil {
				slog.Warn("Failed to close reader body", "err", err)
			}
		}()
	case "deflate":
		reader = flate.NewReader(original)
		defer func() {
			if err := reader.(io.ReadCloser).Close(); err != nil {
				slog.Warn("Failed to close reader body", "err", err)
			}
		}()
	case "br":
		reader = brotli.NewReader(original)
	case "":
		reader = original
	default:
		return nil, http.StatusInternalServerError, fmt.Errorf("unexpected encoding type %v", err)
	}

	body, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return body, 0, nil
}

func encode(encoding string, body []byte) ([]byte, int, error) {
	buf := new(bytes.Buffer)
	var writer io.Writer = buf
	var err error
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(writer)
	case "deflate":
		writer, err = flate.NewWriter(writer, -1)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
	case "br":
		writer = brotli.NewWriter(writer)
	case "":
	default:
		return nil, http.StatusInternalServerError, fmt.Errorf("unexpected encoding type %v", encoding)
	}

	_, err = writer.Write(body)
	if writeCloser, ok := writer.(io.WriteCloser); ok {
		if err := writeCloser.Close(); err != nil {
			slog.Warn("Failed to close writer body", "err", err)
		}
	}
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("unable to encode response %v", err)
	}
	return buf.Bytes(), 0, nil
}

func processData(data io.ReadCloser, encoding string, response []json.RawMessage, responseFn processResponse) ([]json.RawMessage, string, int, error) {
	body, errcode, err := decode(encoding, data)
	if err != nil {
		return nil, "", errcode, fmt.Errorf("unable to decode response %v", err)
	}

	response, token, err := responseFn(body, response)
	if err != nil {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("data processing error %v", err)
	}
	return response, token, 0, nil
}

func loopRequest(req *http.Request, cli *http.Client, responseFn processResponse) ([]json.RawMessage, http.Header, string, int, error) {
	responses := []json.RawMessage{}
	var originalHeader http.Header
	var originalCode int
	var encoding, token string

	for {
		res, err := cli.Do(req)
		if err != nil {
			return nil, nil, "", http.StatusBadRequest, err
		}
		defer func() {
			if err := res.Body.Close(); err != nil {
				slog.Warn("Failed to close response body", "err", err)
			}
		}()
		encoding = res.Header.Get("Content-Encoding")
		originalHeader = res.Header
		originalCode = res.StatusCode

		var errcode int
		responses, token, errcode, err = processData(res.Body, encoding, responses, responseFn)
		if err != nil {
			return nil, nil, "", errcode, err
		}

		if token == "" {
			break
		}
		query := req.URL.Query()
		query.Set("pageToken", token)
		req.URL.RawQuery = query.Encode()
	}
	return responses, originalHeader, encoding, originalCode, nil
}

func buildResponse(responses []json.RawMessage, encoding string) ([]byte, int, error) {
	body, err := json.Marshal(responses)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("response marshaling error %v", err)
	}

	return encode(encoding, body)
}

func (s *Service) setRequestVariables(req *http.Request, subDataSource string) (*http.Client, int, error) {
	slog.Debug("Received resource call", "url", req.URL.String(), "method", req.Method)

	newPath, err := getTarget(req.URL.Path)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	dsInfo, err := s.getDataSourceFromHTTPReq(req)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}

	serviceURL, err := url.Parse(dsInfo.services[subDataSource].url)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	req.URL.Path = newPath
	req.URL.Host = serviceURL.Host
	req.URL.Scheme = serviceURL.Scheme

	return dsInfo.services[subDataSource].client, 0, nil
}

func getTarget(original string) (target string, err error) {
	if original == "/projects" {
		return resourceManagerPath, nil
	}
	splittedPath := strings.SplitN(original, "/", 3)
	if len(splittedPath) < 3 {
		err = fmt.Errorf("the request should contain the service on its path")
		return
	}
	target = fmt.Sprintf("/%s", splittedPath[2])
	return
}

func writeResponseBytes(rw http.ResponseWriter, code int, msg []byte) {
	rw.WriteHeader(code)
	_, err := rw.Write(msg)
	if err != nil {
		slog.Error("Unable to write HTTP response", "error", err)
	}
}

func writeResponse(rw http.ResponseWriter, code int, msg string) {
	writeResponseBytes(rw, code, []byte(msg))
}

func (s *Service) getDataSourceFromHTTPReq(req *http.Request) (*datasourceInfo, error) {
	ctx := req.Context()
	pluginContext := httpadapter.PluginConfigFromContext(ctx)
	i, err := s.im.Get(pluginContext)
	if err != nil {
		return nil, nil
	}
	ds, ok := i.(*datasourceInfo)
	if !ok {
		return nil, fmt.Errorf("unable to convert datasource from service instance")
	}
	return ds, nil
}
