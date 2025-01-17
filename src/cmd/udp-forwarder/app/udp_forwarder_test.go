package app_test

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/cloudfoundry/dropsonde/emitter"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"google.golang.org/grpc"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/loggregator-agent/cmd/udp-forwarder/app"
	"code.cloudfoundry.org/loggregator-agent/internal/testhelper"
	"code.cloudfoundry.org/loggregator-agent/pkg/plumbing"
)

var _ = Describe("UDPForwarder", func() {
	var (
		spyLoggregatorV2Ingress *spyLoggregatorV2Ingress

		// udpPort will be incremented for each test
		udpPort    = 10000
		testLogger = log.New(GinkgoWriter, "", log.LstdFlags)
		testCerts = testhelper.GenerateCerts("loggregatorCA")
	)

	BeforeEach(func() {
		spyLoggregatorV2Ingress = startSpyLoggregatorV2Ingress(testCerts)
	})

	AfterEach(func() {
		gexec.CleanupBuildArtifacts()
		udpPort++
	})

	It("forwards envelopes from Loggregator V1 to V2", func() {
		mc := testhelper.NewMetricClient()
		cfg := app.Config{
			UDPPort: udpPort,
			LoggregatorAgentGRPC: app.GRPC{
				Addr:     spyLoggregatorV2Ingress.addr,
				CAFile:   testCerts.CA(),
				CertFile: testCerts.Cert("metron"),
				KeyFile:  testCerts.Key("metron"),
			},
			Deployment: "test-deployment",
			Job: "test-job",
			Index: "4",
			IP: "127.0.0.1",
		}
		go app.NewUDPForwarder(cfg, testLogger, mc).Run()

		v1e := &events.Envelope{
			Origin:    proto.String("doppler"),
			EventType: events.Envelope_LogMessage.Enum(),
			Timestamp: proto.Int64(time.Now().UnixNano()),
			LogMessage: &events.LogMessage{
				Message:     []byte("some-log-message"),
				MessageType: events.LogMessage_OUT.Enum(),
				Timestamp:   proto.Int64(time.Now().UnixNano()),
			},
		}

		udpEmitter, err := emitter.NewUdpEmitter(fmt.Sprintf("127.0.0.1:%d", udpPort))
		Expect(err).ToNot(HaveOccurred())
		v1Emitter := emitter.NewEventEmitter(udpEmitter, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			v1Emitter.EmitEnvelope(v1e)
			for {
				select {
				case <-ticker.C:
					v1Emitter.EmitEnvelope(v1e)
				case <-ctx.Done():
					return
				}
			}
		}()

		var v2e *loggregator_v2.Envelope
		Eventually(spyLoggregatorV2Ingress.envelopes, 5).Should(Receive(&v2e))
		Expect(string(v2e.GetLog().GetPayload())).To(Equal("some-log-message"))

		Expect(v2e.GetTags()["deployment"]).To(Equal("test-deployment"))
		Expect(v2e.GetTags()["job"]).To(Equal("test-job"))
		Expect(v2e.GetTags()["index"]).To(Equal("4"))
		Expect(v2e.GetTags()["ip"]).To(Equal("127.0.0.1"))
	})
})

type spyLoggregatorV2Ingress struct {
	addr      string
	close     func()
	envelopes chan *loggregator_v2.Envelope
}

func (s *spyLoggregatorV2Ingress) Sender(loggregator_v2.Ingress_SenderServer) error {
	panic("not implemented")
}

func (s *spyLoggregatorV2Ingress) Send(context.Context, *loggregator_v2.EnvelopeBatch) (*loggregator_v2.SendResponse, error) {
	panic("not implemented")
}

func (s *spyLoggregatorV2Ingress) BatchSender(srv loggregator_v2.Ingress_BatchSenderServer) error {
	for {
		batch, err := srv.Recv()
		if err != nil {
			return err
		}

		for _, e := range batch.Batch {
			s.envelopes <- e
		}
	}
}

func startSpyLoggregatorV2Ingress(testCerts *testhelper.TestCerts) *spyLoggregatorV2Ingress {
	s := &spyLoggregatorV2Ingress{
		envelopes: make(chan *loggregator_v2.Envelope, 100),
	}

	serverCreds, err := plumbing.NewServerCredentials(
		testCerts.Cert("metron"),
		testCerts.Key("metron"),
		testCerts.CA(),
	)

	lis, err := net.Listen("tcp", ":0")
	ExpectWithOffset(1, err).ToNot(HaveOccurred())

	grpcServer := grpc.NewServer(grpc.Creds(serverCreds))
	loggregator_v2.RegisterIngressServer(grpcServer, s)

	s.close = func() {
		lis.Close()
	}
	s.addr = lis.Addr().String()

	go grpcServer.Serve(lis)

	return s
}
