package kube2e

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/solo-io/go-utils/stats"

	"github.com/solo-io/gloo/test/gomega/assertions"

	"github.com/solo-io/gloo/test/kube2e/upgrade"

	"go.uber.org/zap/zapcore"

	"github.com/golang/protobuf/proto"

	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/cmd/check"
	"github.com/solo-io/gloo/projects/gloo/cli/pkg/cmd/options"
	clienthelpers "github.com/solo-io/gloo/projects/gloo/cli/pkg/helpers"
	v1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	"github.com/solo-io/k8s-utils/kubeutils"
	"github.com/solo-io/k8s-utils/testutils/helper"
	"github.com/solo-io/solo-kit/pkg/api/v1/clients"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	. "github.com/onsi/gomega"
	errors "github.com/rotisserie/eris"
	"k8s.io/client-go/kubernetes"
)

func GetHttpEchoImage() string {
	httpEchoImage := "hashicorp/http-echo"
	if runtime.GOARCH == "arm64" {
		httpEchoImage = "gcr.io/solo-test-236622/http-echo"
	}
	return httpEchoImage
}

func MustKubeClient() kubernetes.Interface {
	restConfig, err := kubeutils.GetConfig("", "")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return kubeClient
}

// GlooctlCheckEventuallyHealthy will run up until proved timeoutInterval or until gloo is reported as healthy
func GlooctlCheckEventuallyHealthy(offset int, testHelper *helper.SoloTestHelper, timeoutInterval string) {
	EventuallyWithOffset(offset, func() error {
		contextWithCancel, cancel := context.WithCancel(context.Background())
		defer cancel()
		opts := &options.Options{
			Metadata: core.Metadata{
				Namespace: testHelper.InstallNamespace,
			},
			Top: options.Top{
				Ctx: contextWithCancel,
			},
		}
		err := check.CheckResources(opts)
		if err != nil {
			return errors.Wrap(err, "glooctl check detected a problem with the installation")
		}
		return nil
	}, timeoutInterval, "5s").Should(BeNil())
}

func GetHelmValuesOverrideFile() (filename string, cleanup func()) {
	values, err := ioutil.TempFile("", "values-*.yaml")
	Expect(err).NotTo(HaveOccurred())

	// disabling usage statistics is not important to the functionality of the tests,
	// but we don't want to report usage in CI since we only care about how our users are actually using Gloo.
	// install to a single namespace so we can run multiple invocations of the regression tests against the
	// same cluster in CI.
	_, err = values.Write([]byte(`
global:
  image:
    pullPolicy: IfNotPresent
  glooRbac:
    namespaced: true
    nameSuffix: e2e-test-rbac-suffix
settings:
  singleNamespace: true
  create: true
  invalidConfigPolicy:
    replaceInvalidRoutes: true
    invalidRouteResponseCode: 404
    invalidRouteResponseBody: Gloo Gateway has invalid configuration.
gateway:
  persistProxySpec: true
gloo:
  deployment:
    replicas: 2
    customEnv:
      - name: LEADER_ELECTION_LEASE_DURATION
        value: 4s
    livenessProbeEnabled: true
gatewayProxies:
  gatewayProxy:
    healthyPanicThreshold: 0
`))
	Expect(err).NotTo(HaveOccurred())

	err = values.Close()
	Expect(err).NotTo(HaveOccurred())

	return values.Name(), func() { _ = os.Remove(values.Name()) }
}

func EventuallyReachesConsistentState(installNamespace string) {
	// We port-forward the Gloo deployment stats port to inspect the metrics and log settings
	glooStatsForwardConfig := assertions.StatsPortFwd{
		ResourceName:      "deployment/gloo",
		ResourceNamespace: installNamespace,
		LocalPort:         stats.DefaultPort,
		TargetPort:        stats.DefaultPort,
	}

	// Gloo components are configured to log to the Info level by default
	logLevelAssertion := assertions.LogLevelAssertion(zapcore.InfoLevel)

	// The emitter at some point should stabilize and not continue to increase the number of snapshots produced
	// We choose 4 here as a bit of a magic number, but we feel comfortable that if 4 consecutive polls of the metrics
	// endpoint returns that same value, then we have stabilized
	identicalResultInARow := 4
	emitterMetricAssertion, _ := assertions.IntStatisticReachesConsistentValueAssertion("api_gloosnapshot_gloo_solo_io_emitter_snap_out", identicalResultInARow)

	ginkgo.By("Gloo eventually reaches a consistent state")
	offset := 1 // This method is called directly from a TestSuite
	assertions.EventuallyWithOffsetStatisticsMatchAssertions(offset, glooStatsForwardConfig,
		logLevelAssertion.WithOffset(offset),
		emitterMetricAssertion.WithOffset(offset),
	)
}

// Copied from: https://github.com/solo-io/go-utils/blob/176c4c008b4d7cde836269c7a817f657b6981236/testutils/assertions.go#L20
func ExpectEqualProtoMessages(g Gomega, a, b proto.Message, optionalDescription ...interface{}) {
	if proto.Equal(a, b) {
		return
	}

	g.Expect(a.String()).To(Equal(b.String()), optionalDescription...)
}

func UpdateDisableTransformationValidationSetting(ctx context.Context, shouldDisable bool, installNamespace string) {
	UpdateSettings(ctx, func(settings *v1.Settings) {
		Expect(settings.GetGateway().GetValidation()).NotTo(BeNil())
		settings.GetGateway().GetValidation().DisableTransformationValidation = &wrappers.BoolValue{Value: shouldDisable}
	}, installNamespace)
}

// enable/disable strict validation
func UpdateAlwaysAcceptSetting(ctx context.Context, alwaysAccept bool, installNamespace string) {
	UpdateSettings(ctx, func(settings *v1.Settings) {
		Expect(settings.GetGateway().GetValidation()).NotTo(BeNil())
		settings.GetGateway().GetValidation().AlwaysAccept = &wrappers.BoolValue{Value: alwaysAccept}
	}, installNamespace)
}

func UpdateRestEdsSetting(ctx context.Context, enableRestEds bool, installNamespace string) {
	UpdateSettings(ctx, func(settings *v1.Settings) {
		Expect(settings.GetGloo()).NotTo(BeNil())
		settings.GetGloo().EnableRestEds = &wrappers.BoolValue{Value: enableRestEds}
	}, installNamespace)
}

func UpdateReplaceInvalidRoutes(ctx context.Context, replaceInvalidRoutes bool, installNamespace string) {
	UpdateSettings(ctx, func(settings *v1.Settings) {
		Expect(settings.GetGloo().GetInvalidConfigPolicy()).NotTo(BeNil())
		settings.GetGloo().GetInvalidConfigPolicy().ReplaceInvalidRoutes = replaceInvalidRoutes
	}, installNamespace)
}

func UpdateSettings(ctx context.Context, updateSettings func(settings *v1.Settings), installNamespace string) {
	// when validation config changes, the validation server restarts -- give time for it to come up again.
	// without the wait, the validation webhook may temporarily fallback to it's failurePolicy, which is not
	// what we want to test.
	// TODO (samheilbron) We should avoid relying on time.Sleep in our tests as these tend to cause flakes
	waitForSettingsToPropagate := func() {
		time.Sleep(3 * time.Second)
	}
	UpdateSettingsWithPropagationDelay(updateSettings, waitForSettingsToPropagate, ctx, installNamespace)
}

func UpdateSettingsWithPropagationDelay(updateSettings func(settings *v1.Settings), waitForSettingsToPropagate func(), ctx context.Context, installNamespace string) {
	settingsClient := clienthelpers.MustSettingsClient(ctx)
	settings, err := settingsClient.Read(installNamespace, "default", clients.ReadOpts{})
	Expect(err).NotTo(HaveOccurred())

	updateSettings(settings)

	_, err = settingsClient.Write(settings, clients.WriteOpts{OverwriteExisting: true})
	Expect(err).NotTo(HaveOccurred())

	waitForSettingsToPropagate()
}

func ToFile(content string) string {
	f, err := ioutil.TempFile("", "")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	n, err := f.WriteString(content)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, n).To(Equal(len(content)))
	_ = f.Close()
	return f.Name()
}

// https://github.com/solo-io/gloo/issues/4043#issuecomment-772706604
// We should move tests away from using the testrunner, and instead depend on EphemeralContainers.
// The default response changed in later kube versions, which caused this value to change.
// Ideally the test utilities used by Gloo are maintained in the Gloo repo, so I opted to move
// this constant here.
// This response is given by the testrunner when the SimpleServer is started
func GetSimpleTestRunnerHttpResponse() string {
	if runtime.GOARCH == "arm64" {
		return SimpleTestRunnerHttpResponseArm
	} else {
		return SimpleTestRunnerHttpResponse
	}
}

// For nightly runs, we want to install a released version rather than using a locally built chart
// To do this, set the environment variable RELEASED_VERSION with either a version name or "LATEST" to get the last release
func GetTestReleasedVersion(ctx context.Context, repoName string) string {
	var useVersion string
	if useVersion = os.Getenv("RELEASED_VERSION"); useVersion != "" {
		if useVersion == "LATEST" {
			_, current, err := upgrade.GetUpgradeVersions(ctx, repoName)
			Expect(err).NotTo(HaveOccurred())
			useVersion = current.String()
		}
	}
	return useVersion
}
func GetTestHelper(ctx context.Context, namespace string) (*helper.SoloTestHelper, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if useVersion := GetTestReleasedVersion(ctx, "gloo"); useVersion != "" {
		return helper.NewSoloTestHelper(func(defaults helper.TestConfig) helper.TestConfig {
			defaults.RootDir = filepath.Join(cwd, "../../..")
			defaults.HelmChartName = "gloo"
			defaults.InstallNamespace = namespace
			defaults.ReleasedVersion = useVersion
			defaults.Verbose = true
			return defaults
		})
	} else {
		return helper.NewSoloTestHelper(func(defaults helper.TestConfig) helper.TestConfig {
			defaults.RootDir = filepath.Join(cwd, "../../..")
			defaults.HelmChartName = "gloo"
			defaults.InstallNamespace = namespace
			defaults.Verbose = true
			return defaults
		})
	}
}

const SimpleTestRunnerHttpResponse = `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 3.2 Final//EN"><html>
<title>Directory listing for /</title>
<body>
<h2>Directory listing for /</h2>
<hr>
<ul>
<li><a href="bin/">bin/</a>
<li><a href="boot/">boot/</a>
<li><a href="dev/">dev/</a>
<li><a href="etc/">etc/</a>
<li><a href="home/">home/</a>
<li><a href="lib/">lib/</a>
<li><a href="lib64/">lib64/</a>
<li><a href="media/">media/</a>
<li><a href="mnt/">mnt/</a>
<li><a href="opt/">opt/</a>
<li><a href="proc/">proc/</a>
<li><a href="product_name">product_name</a>
<li><a href="product_uuid">product_uuid</a>
<li><a href="root/">root/</a>
<li><a href="root.crt">root.crt</a>
<li><a href="run/">run/</a>
<li><a href="sbin/">sbin/</a>
<li><a href="srv/">srv/</a>
<li><a href="sys/">sys/</a>
<li><a href="tmp/">tmp/</a>
<li><a href="usr/">usr/</a>
<li><a href="var/">var/</a>
</ul>
<hr>
</body>
</html>`

const SimpleTestRunnerHttpResponseArm = `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 3.2 Final//EN"><html>
<title>Directory listing for /</title>
<body>
<h2>Directory listing for /</h2>
<hr>
<ul>
<li><a href="bin/">bin/</a>
<li><a href="boot/">boot/</a>
<li><a href="dev/">dev/</a>
<li><a href="etc/">etc/</a>
<li><a href="home/">home/</a>
<li><a href="lib/">lib/</a>
<li><a href="lib64/">lib64/</a>
<li><a href="media/">media/</a>
<li><a href="mnt/">mnt/</a>
<li><a href="opt/">opt/</a>
<li><a href="proc/">proc/</a>
<li><a href="product_uuid">product_uuid</a>
<li><a href="root/">root/</a>
<li><a href="root.crt">root.crt</a>
<li><a href="run/">run/</a>
<li><a href="sbin/">sbin/</a>
<li><a href="srv/">srv/</a>
<li><a href="sys/">sys/</a>
<li><a href="tmp/">tmp/</a>
<li><a href="usr/">usr/</a>
<li><a href="var/">var/</a>
</ul>
<hr>
</body>
</html>`
