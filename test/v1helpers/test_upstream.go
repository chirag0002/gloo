package v1helpers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/solo-io/gloo/test/gomega/matchers"

	"github.com/golang/protobuf/ptypes/wrappers"

	"github.com/golang/protobuf/proto"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	gloov1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	static_plugin_gloo "github.com/solo-io/gloo/projects/gloo/pkg/api/v1/options/static"
	"github.com/solo-io/gloo/test/helpers"
	testgrpcservice "github.com/solo-io/gloo/test/v1helpers/test_grpc_service"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type ReceivedRequest struct {
	Method      string
	Headers     map[string][]string
	URL         *url.URL
	Body        []byte
	Host        string
	GRPCRequest proto.Message
	Port        uint32
}

const (
	NO_TLS = iota
	TLS
	MTLS
)

type UpstreamTlsRequired int

func NewTestHttpUpstream(ctx context.Context, addr string) *TestUpstream {
	backendPort, responses := runTestServer(ctx, "", NO_TLS)
	return newTestUpstream(addr, []uint32{backendPort}, responses)
}

func NewTestHttpUpstreamWithTls(ctx context.Context, addr string, tlsServer UpstreamTlsRequired) *TestUpstream {
	backendPort, responses := runTestServer(ctx, "", tlsServer)
	return newTestUpstream(addr, []uint32{backendPort}, responses)
}

func NewTestHttpUpstreamWithReply(ctx context.Context, addr, reply string) *TestUpstream {
	backendPort, responses := runTestServer(ctx, reply, NO_TLS)
	return newTestUpstream(addr, []uint32{backendPort}, responses)
}

func NewTestHttpUpstreamWithReplyAndHealthReply(ctx context.Context, addr, reply, healthReply string) *TestUpstream {
	backendPort, responses := runTestServerWithHealthReply(ctx, reply, healthReply, NO_TLS)
	return newTestUpstream(addr, []uint32{backendPort}, responses)
}

func NewTestHttpsUpstreamWithReply(ctx context.Context, addr, reply string) *TestUpstream {
	backendPort, responses := runTestServer(ctx, reply, TLS)
	return newTestUpstream(addr, []uint32{backendPort}, responses)
}

func NewTestGRPCUpstream(ctx context.Context, addr string, replicas int) *TestUpstream {
	grpcServices := make([]*testgrpcservice.TestGRPCServer, replicas)
	for i := range grpcServices {
		grpcServices[i] = testgrpcservice.RunServer(ctx)
	}
	received := make(chan *ReceivedRequest, 100)
	for _, srv := range grpcServices {
		srv := srv
		go func() {
			defer GinkgoRecover()
			for r := range srv.C {
				received <- &ReceivedRequest{GRPCRequest: r, Port: srv.Port}
			}
		}()
	}
	ports := make([]uint32, 0, len(grpcServices))
	for _, v := range grpcServices {
		ports = append(ports, v.Port)
	}

	us := newTestUpstream(addr, ports, received)
	us.Upstream.UseHttp2 = &wrappers.BoolValue{Value: true}
	us.GrpcServers = grpcServices
	return us
}

type TestUpstream struct {
	Upstream    *gloov1.Upstream
	C           <-chan *ReceivedRequest
	Address     string
	Port        uint32
	GrpcServers []*testgrpcservice.TestGRPCServer
}

func (tu *TestUpstream) FailGrpcHealthCheck() *testgrpcservice.TestGRPCServer {
	for _, v := range tu.GrpcServers[:len(tu.GrpcServers)-1] {
		v.HealthChecker.Fail()
	}
	return tu.GrpcServers[len(tu.GrpcServers)-1]
}

var id = 0

func newTestUpstream(addr string, ports []uint32, responses <-chan *ReceivedRequest) *TestUpstream {
	id += 1
	hosts := make([]*static_plugin_gloo.Host, len(ports))
	for i, port := range ports {
		hosts[i] = &static_plugin_gloo.Host{
			Addr: addr,
			Port: port,
		}
	}
	u := &gloov1.Upstream{
		Metadata: &core.Metadata{
			Name:      fmt.Sprintf("local-test-upstream-%d", id),
			Namespace: "default",
		},
		UpstreamType: &gloov1.Upstream_Static{
			Static: &static_plugin_gloo.UpstreamSpec{
				Hosts: hosts,
			},
		},
	}

	return &TestUpstream{
		Upstream: u,
		C:        responses,
		Port:     ports[0],
	}
}

func runTestServer(ctx context.Context, reply string, tlsServer UpstreamTlsRequired) (uint32, <-chan *ReceivedRequest) {
	return runTestServerWithHealthReply(ctx, reply, "OK", tlsServer)
}

func runTestServerWithHealthReply(ctx context.Context, reply, healthReply string, tlsServer UpstreamTlsRequired) (uint32, <-chan *ReceivedRequest) {
	bodyChan := make(chan *ReceivedRequest, 100)
	handlerFunc := func(rw http.ResponseWriter, r *http.Request) {
		var rr ReceivedRequest
		rr.Method = r.Method

		var body []byte
		if r.Body != nil {
			body, _ = ioutil.ReadAll(r.Body)
			_ = r.Body.Close()
			if len(body) != 0 {
				rr.Body = body
			}
		}

		if reply != "" {
			_, _ = rw.Write([]byte(reply))
		} else if body != nil {
			_, _ = rw.Write(body)
		}
		rr.Host = r.Host
		rr.URL = r.URL
		rr.Headers = r.Header

		bodyChan <- &rr
	}

	listener, err := getListener(tlsServer)
	if err != nil {
		panic(err)
	}

	addr := listener.Addr().String()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(handlerFunc))
	mux.Handle("/health", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(healthReply))
	}))

	go func() {
		defer GinkgoRecover()
		h := &http.Server{Handler: mux}

		go func() {
			defer GinkgoRecover()
			if err := h.Serve(listener); err != nil {
				if err != http.ErrServerClosed {
					panic(err)
				}
			}
		}()

		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_ = h.Shutdown(ctx)
		cancel()
		// close channel, the http handler may panic but this should be caught by the http code.
		close(bodyChan)
	}()
	return uint32(port), bodyChan
}

func getListener(tlsServer UpstreamTlsRequired) (net.Listener, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}

	if tlsServer > NO_TLS {
		fmt.Fprintln(GinkgoWriter, "test server serving tls")
		certGenFunc, keyGenFunc := helpers.Certificate, helpers.PrivateKey
		if tlsServer == MTLS {
			fmt.Fprintln(GinkgoWriter, "test server serving mtls")
			certGenFunc, keyGenFunc = helpers.MtlsCertificate, helpers.MtlsPrivateKey
		}
		cert, key := certGenFunc(), keyGenFunc()
		certs, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return nil, err
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certs},
		}
		if tlsServer == MTLS {
			certPool := x509.NewCertPool()
			certPool.AppendCertsFromPEM([]byte(cert))
			tlsConfig.ClientCAs = certPool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
		listener = tls.NewListener(listener, tlsConfig)
	}
	return listener, nil
}

func TestUpstreamReachable(envoyPort uint32, tu *TestUpstream, rootca *string) {
	TestUpstreamReachableWithOffset(2, envoyPort, tu, rootca)
}

func TestUpstreamReachableWithOffset(offset int, envoyPort uint32, tu *TestUpstream, rootca *string) {
	body := []byte("solo.io test")

	ExpectHttpOK(body, rootca, envoyPort, "solo.io test")

	timeout := time.After(15 * time.Second)
	var receivedRequest *ReceivedRequest
	for {
		select {
		case <-timeout:
			if receivedRequest != nil {
				fmt.Fprintf(GinkgoWriter, "last received request: %v", *receivedRequest)
			}
			Fail("timeout testing upstream reachability")
		case receivedRequest = <-tu.C:
			if receivedRequest.Method == "POST" &&
				bytes.Equal(receivedRequest.Body, body) {
				return
			}
		}
	}

}

func ExpectHttpOK(body []byte, rootca *string, envoyPort uint32, response string) {
	ExpectHttpOKWithOffset(1, body, rootca, envoyPort, response)
}

func ExpectHttpOKWithOffset(offset int, body []byte, rootca *string, envoyPort uint32, response string) {
	ExpectHttpStatusWithOffset(offset+1, body, rootca, envoyPort, response, http.StatusOK)
}

func ExpectHttpUnavailableWithOffset(offset int, body []byte, rootca *string, envoyPort uint32, response string) {
	ExpectHttpStatusWithOffset(offset+1, body, rootca, envoyPort, response, http.StatusServiceUnavailable)
}

func ExpectHttpStatusWithOffset(offset int, body []byte, rootca *string, envoyPort uint32, response string, status int) {
	ExpectCurlWithOffset(
		offset+1,
		CurlRequest{
			RootCA: rootca,
			Port:   envoyPort,
			Path:   "/1",
			Body:   body,
		},
		CurlResponse{
			Message: response,
			Status:  status,
		})
}

type CurlRequest struct {
	RootCA  *string
	Port    uint32
	Path    string
	Body    []byte
	Host    string
	Headers map[string]string
}

type CurlResponse struct {
	Status  int
	Message string
}

func ExpectCurlWithOffset(offset int, request CurlRequest, expectedResponse CurlResponse) {

	EventuallyWithOffset(offset+1, func(g Gomega) {
		// send a request with a body
		var buf bytes.Buffer
		buf.Write(request.Body)

		var client http.Client

		scheme := "http"
		if request.RootCA != nil {
			scheme = "https"
			caCertPool := x509.NewCertPool()
			ok := caCertPool.AppendCertsFromPEM([]byte(*request.RootCA))
			g.Expect(ok).To(BeTrue())

			tlsConfig := &tls.Config{
				RootCAs:            caCertPool,
				InsecureSkipVerify: true,
			}
			client.Transport = &http.Transport{
				TLSClientConfig: tlsConfig,
			}
		}

		requestUrl := fmt.Sprintf("%s://%s:%d%s", scheme, "localhost", request.Port, request.Path)
		req, err := http.NewRequest(http.MethodPost, requestUrl, &buf)
		g.Expect(err).NotTo(HaveOccurred())

		if request.Host != "" {
			req.Host = request.Host
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		for headerName, headerValue := range request.Headers {
			req.Header.Set(headerName, headerValue)
		}

		g.Expect(client.Do(req)).Should(matchers.HaveHttpResponse(&matchers.HttpResponse{
			StatusCode: expectedResponse.Status,
			Body:       expectedResponse.Message,
		}))
	}, "30s", "1s").Should(Succeed())
}

func ExpectGrpcHealthOK(rootca *string, envoyPort uint32, service string) {
	EventuallyWithOffset(2, func() error {
		// send a request with a body

		opts := []grpc.DialOption{grpc.WithBlock()}
		if rootca != nil {
			caCertPool := x509.NewCertPool()
			ok := caCertPool.AppendCertsFromPEM([]byte(*rootca))
			if !ok {
				Fail("ca cert is not OK")
			}
			creds := credentials.NewTLS(&tls.Config{
				ClientCAs:          caCertPool,
				InsecureSkipVerify: true,
			})

			opts = append(opts, grpc.WithTransportCredentials(creds))
		} else {
			opts = append(opts, grpc.WithInsecure())
		}
		conn, err := grpc.Dial(fmt.Sprintf("%s:%d", "localhost", envoyPort), opts...)
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		defer conn.Close()

		c := healthpb.NewHealthClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, err := c.Check(ctx, &healthpb.HealthCheckRequest{Service: service})
		cancel()
		if err != nil {
			return err
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			return fmt.Errorf("%v is not SERVING", resp.GetStatus())
		}
		return nil
	}, "30s", "1s").Should(BeNil())
}
