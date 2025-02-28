/*
Copyright 2019 The Kubernetes Authors.
Copyright 2020 Authors of SuperEdge - file modified.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubeadm

import (
	"fmt"
	"github.com/superedge/edgeadm/pkg/util/kubeclient"
	"io"
	kubeadmapiv1beta3 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta3"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/lithammer/dedent"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmscheme "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	"k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/validation"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	phases "k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/init"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/features"
	certsphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/certs"
	kubeconfigphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubeconfig"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	configutil "k8s.io/kubernetes/cmd/kubeadm/app/util/config"
	kubeconfigutil "k8s.io/kubernetes/cmd/kubeadm/app/util/kubeconfig"

	"github.com/superedge/edgeadm/pkg/edgeadm/cmd"
	"github.com/superedge/edgeadm/pkg/edgeadm/common"
	"github.com/superedge/edgeadm/pkg/edgeadm/constant"
	"github.com/superedge/edgeadm/pkg/edgeadm/steps"
	"github.com/superedge/edgeadm/pkg/util"
)

var (
	initDoneTempl = template.Must(template.New("init").Parse(dedent.Dedent(`
		Your Kubernetes control-plane has initialized successfully!

		To start using your cluster, you need to run the following as a regular user:

		  mkdir -p $HOME/.kube
		  sudo cp -i {{.KubeConfigPath}} $HOME/.kube/config
		  sudo chown $(id -u):$(id -g) $HOME/.kube/config

		You should now deploy a pod network to the cluster.
		Run "kubectl apply -f [podnetwork].yaml" with one of the options listed at:
		  https://kubernetes.io/docs/concepts/cluster-administration/addons/

		{{if .ControlPlaneEndpoint -}}
		{{if .UploadCerts -}}
		You can now join any number of the control-plane node running the following command on each as root:

		  {{.joinControlPlaneCommand}}

		Please note that the certificate-key gives access to cluster sensitive data, keep it secret!
		As a safeguard, uploaded-certs will be deleted in two hours; If necessary, you can use
		"edgeadm init phase upload-certs --upload-certs" to reload certs afterward.

		{{else -}}
		You can now join any number of control-plane nodes by copying certificate authorities
		and service account keys on each node and then running the following as root:

		  {{.joinControlPlaneCommand}}

		{{end}}{{end}}Then you can join any number of worker nodes by running the following on each as root:

		{{.joinWorkerCommand}}
		`)))
)

// initOptions defines all the init options exposed via flags by kubeadm init.
// Please note that this structure includes the public kubeadm config API, but only a subset of the options
// supported by this api will be exposed as a flag.
type initOptions struct {
	cfgPath                 string
	skipTokenPrint          bool
	dryRun                  bool
	kubeconfigDir           string
	kubeconfigPath          string
	featureGatesString      string
	ignorePreflightErrors   []string
	bto                     *options.BootstrapTokenOptions
	externalInitCfg         *kubeadmapiv1beta3.InitConfiguration
	externalClusterCfg      *kubeadmapiv1beta3.ClusterConfiguration
	uploadCerts             bool
	skipCertificateKeyPrint bool
	kustomizeDir            string
	patchesDir              string
}

// compile-time assert that the local data object satisfies the phases data interface.
var _ phases.InitData = &initData{}

// initData defines all the runtime information used when running the kubeadm init workflow;
// this data is shared across all the phases that are included in the workflow.
type initData struct {
	cfg                     *kubeadmapi.InitConfiguration
	skipTokenPrint          bool
	dryRun                  bool
	kubeconfigDir           string
	kubeconfigPath          string
	ignorePreflightErrors   sets.String
	certificatesDir         string
	dryRunDir               string
	externalCA              bool
	client                  clientset.Interface
	outputWriter            io.Writer
	uploadCerts             bool
	skipCertificateKeyPrint bool
	kustomizeDir            string
	patchesDir              string
}

func NewInitCMD(out io.Writer, edgeConfig *cmd.EdgeadmConfig) *cobra.Command {
	initOptions := newInitOptions()
	initRunner := workflow.NewRunner()

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run this command in order to set up the Kubernetes control plane",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := initRunner.InitData(args)
			if err != nil {
				return err
			}

			data := c.(*initData)
			fmt.Printf("[init] Deploy Kubernetes kubeadm-config: %s\n", util.ToJson(data.cfg))

			if err := initRunner.Run(args); err != nil {
				return err
			}

			return showJoinCommand(data, out)
		},
		Args: cobra.NoArgs,
	}

	// One-click install of flags for cluster
	initClusterFlags(cmd.Flags(), edgeConfig)
	initInstallHAFlag(cmd.Flags(), edgeConfig)
	initContainerRuntimeFlags(cmd.Flags(), edgeConfig)

	// adds flags to the init command
	// init command local flags could be eventually inherited by the sub-commands automatically generated for phases
	AddInitConfigFlags(cmd.Flags(), initOptions.externalInitCfg)
	AddClusterConfigFlags(cmd.Flags(), initOptions.externalClusterCfg, &initOptions.featureGatesString)
	AddInitOtherFlags(cmd.Flags(), initOptions)
	initOptions.bto.AddTokenFlag(cmd.Flags())
	initOptions.bto.AddTTLFlag(cmd.Flags())
	options.AddImageMetaFlags(cmd.Flags(), &initOptions.externalClusterCfg.ImageRepository)

	// defines additional flag that are not used by the init command but that could be eventually used
	// by the sub-commands automatically generated for phases
	initRunner.SetAdditionalFlags(func(flags *flag.FlagSet) {
		options.AddKubeConfigFlag(flags, &initOptions.kubeconfigPath)
		options.AddKubeConfigDirFlag(flags, &initOptions.kubeconfigDir)
		options.AddControlPlanExtraArgsFlags(flags, &initOptions.externalClusterCfg.APIServer.ExtraArgs, &initOptions.externalClusterCfg.ControllerManager.ExtraArgs, &initOptions.externalClusterCfg.Scheduler.ExtraArgs)
	})

	// default config
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if strings.Contains(edgeConfig.InstallPkgPath, constant.InstallPkgPathNote) {
			return fmt.Errorf("Please enter --%s value(path of edgeadm static install package)\n", constant.InstallPkgPath)
		}

		// set one-click install static file
		dstInstallPackagePath := edgeConfig.WorkerPath + constant.EdgeamdDir
		if err := common.UnzipPackage(edgeConfig.InstallPkgPath, dstInstallPackagePath); err != nil {
			klog.Errorf("Unzip package: %s, error: %v", edgeConfig.InstallPkgPath, err)
			return err
		}
		if initOptions.kubeconfigDir == "" {
			initOptions.kubeconfigDir = constant.KubeCfgPath
		}
		if err := common.SetPackagePath(edgeConfig.WorkerPath, initOptions.kubeconfigDir); err != nil {
			klog.Errorf("Unzip package: %s, error: %v", edgeConfig.InstallPkgPath, err)
			return err
		}

		//schduelr config
		kubeSchedulerConfDir := initOptions.kubeconfigDir + "/"
		os.MkdirAll(path.Dir(kubeSchedulerConfDir), 0755)
		moveKubeSchedulerConf := fmt.Sprintf("mv -f %s %s", edgeConfig.WorkerPath+constant.KubeSchedulerConf, kubeSchedulerConfDir)
		if _, _, err := util.RunLinuxCommand(moveKubeSchedulerConf); err != nil {
			return err
		}

		// set edgeadm crisocket for runtime
		switch edgeConfig.ContainerRuntime {
		case constant.ContainerRuntimeDocker:
			initOptions.externalInitCfg.NodeRegistration.CRISocket = constant.DefaultDockerCRISocket
		case constant.ContainerRuntimeContainerd:
			initOptions.externalInitCfg.NodeRegistration.CRISocket = constant.DefaultContainerdCRISocket
		default:
			return fmt.Errorf("Container runtime support 'docker' and 'containerd', not %s\n", edgeConfig.ContainerRuntime)
		}

		// set one-click install kubernetes config
		if edgeConfig.IsEnableEdge {
			if err := edgeadmConfigUpdate(initOptions, edgeConfig); err != nil {
				klog.Errorf("Init edgeadm config error: %v", err)
				return err
			}
		}
		steps.EdgeadmConf = edgeConfig
		klog.V(2).Infof("Get one-click install kubernetes config: %s", util.ToJson(steps.EdgeadmConf))

		return nil
	}

	// Initialize the node before one-click install kubernetes
	initRunner.AppendPhase(steps.NewInitNodePhase())
	initRunner.AppendPhase(steps.NewContainerPhase())

	// initialize the workflow runner with the list of phases
	initRunner.AppendPhase(phases.NewPreflightPhase())
	initRunner.AppendPhase(phases.NewCertsPhase())
	initRunner.AppendPhase(NewEdgeCertsPhase(edgeConfig))
	initRunner.AppendPhase(phases.NewKubeConfigPhase())
	initRunner.AppendPhase(phases.NewKubeletStartPhase())
	initRunner.AppendPhase(steps.NewKubeVIPInitPhase(edgeConfig))
	initRunner.AppendPhase(NewEdgeConfPhase(edgeConfig))
	initRunner.AppendPhase(phases.NewControlPlanePhase())
	initRunner.AppendPhase(phases.NewEtcdPhase())
	initRunner.AppendPhase(phases.NewWaitControlPlanePhase())
	initRunner.AppendPhase(phases.NewUploadConfigPhase())
	initRunner.AppendPhase(phases.NewUploadCertsPhase())
	initRunner.AppendPhase(phases.NewMarkControlPlanePhase())
	initRunner.AppendPhase(phases.NewBootstrapTokenPhase())
	initRunner.AppendPhase(phases.NewKubeletFinalizePhase())
	initRunner.AppendPhase(phases.NewAddonPhase())

	// Addon network designed for Kubernetes
	initRunner.AppendPhase(steps.NewCNINetworkAppsPhase())
	// Addon SuperEdge edge-apps to Kubernetes cluster
	initRunner.AppendPhase(steps.NewEdgeAppsPhase(edgeConfig))

	// sets the data builder function, that will be used by the runner
	// both when running the entire workflow or single phases
	initRunner.SetDataInitializer(func(cmd *cobra.Command, args []string) (workflow.RunData, error) {
		return newInitData(cmd, args, initOptions, out)
	})

	// binds the Runner to kubeadm init command by altering
	// command help, adding --skip-phases flag and by adding phases subcommands
	initRunner.BindToCommand(cmd)

	return cmd
}

func initClusterFlags(flagSet *flag.FlagSet, edgeConfig *cmd.EdgeadmConfig) {
	flagSet.StringVar(
		&edgeConfig.InstallPkgPath, constant.InstallPkgPath,
		constant.InstallPkgNetworkLocation, "Install static package path of kubernetes cluster.",
	)

	flagSet.StringVar(
		&edgeConfig.ManifestsDir, constant.ManifestsDir, "", "Manifests document of edge kubernetes cluster.",
	)

	flagSet.StringVar(
		&edgeConfig.Version, constant.EdgeVersion, constant.Version, "Superedge realted images' version.",
	)

	flagSet.StringVar(
		&edgeConfig.EdgeImageRepository, constant.EdgeImageRepository, constant.ImageRepository, "Superedge related images registry, seperated from the default --image-repository (k8s.gcr.io).",
	)

	flagSet.StringVar(
		&edgeConfig.EdgeVirtualAddr, constant.EdgeVirtualAddr, constant.DefaultEdgeVirtualAddr, "Superedge related images registry, seperated from the default --image-repository (k8s.gcr.io).",
	)

}

func initContainerRuntimeFlags(flagSet *flag.FlagSet, edgeConfig *cmd.EdgeadmConfig) {
	flagSet.StringVar(
		&edgeConfig.ContainerRuntime, constant.ContainerRuntime,
		constant.ContainerRuntimeContainerd, "Container runtime support docker and containerd.",
	)
}

func initInstallHAFlag(flagSet *flag.FlagSet, edgeConfig *cmd.EdgeadmConfig) {
	flagSet.StringVar(
		&edgeConfig.DefaultHA, constant.DefaultHA,
		constant.DefaultHAKubeVIP, `Specify a HA to install`,
	)
	flagSet.StringVar(
		&edgeConfig.KubeVIPInterface, constant.HANetworkInterface,
		constant.HANetworkDefaultInterface, `Specify a network interface for the control-plane-endpoint to install default HA.`,
	)
}

func edgeadmConfigUpdate(initOptions *initOptions, edgeadmConfig *cmd.EdgeadmConfig) error {
	// edgeadm default value
	initOptions.externalClusterCfg.APIServer.ExtraArgs = map[string]string{
		"kubelet-preferred-address-types": "Hostname",
		"egress-selector-config-file":     "/etc/kubernetes/kube-apiserver-conf/egress-selector-configuration.yaml",
		"enable-aggregator-routing":       "true",
	}
	initOptions.externalClusterCfg.APIServer.ExtraVolumes = []kubeadmapiv1beta3.HostPathMount{
		{
			Name:      "kube-apiserver-conf",
			HostPath:  "/etc/kubernetes/kube-apiserver-conf",
			MountPath: "/etc/kubernetes/kube-apiserver-conf",
			ReadOnly:  false,
			PathType:  v1.HostPathDirectoryOrCreate,
		},
	}
	clusterConfig := initOptions.externalClusterCfg

	option := map[string]interface{}{}
	kubeAPIServerPatch, err := kubeclient.ParseString(constant.KubeAPIServerPatchYaml, option)
	if err != nil {
		klog.Errorf("Parse %s yaml: %s, option: %v, error: %v", constant.KubeAPIServerPatch, option, err)
		return err
	}

	// kube-scheduler
	if util.IsFileExist(constant.SchedulerConfig) && util.IsFileExist(constant.SchedulerPolicy) {
		schedulerConfig := clusterConfig.Scheduler
		if len(schedulerConfig.ExtraArgs) == 0 {
			schedulerConfig.ExtraArgs = make(map[string]string)
		}
		//In order to be compatible with more K8s versions, remove the topolvm extended scheduler configuration.
		//Because 1.20 and above does not have a KubeSchedulerConfiguration object. Edge into manual support.
		//schedulerConfig.ExtraArgs["config"] = constant.SchedulerConfig
		//schedulerConfig.ExtraArgs["policy-config-file"] = constant.SchedulerPolicy
		schedulerConfig.ExtraVolumes = append(schedulerConfig.ExtraVolumes, []kubeadmapiv1beta3.HostPathMount{
			{
				"kube-scheduler-config",
				constant.SchedulerConfig,
				constant.SchedulerConfig,
				true,
				v1.HostPathFileOrCreate,
			},
			{
				"kube-scheduler-policy",
				constant.SchedulerPolicy,
				constant.SchedulerPolicy,
				true,
				v1.HostPathFileOrCreate,
			},
		}...)
		clusterConfig.Scheduler = schedulerConfig
	}

	// kube-* patch config
	if initOptions.patchesDir == "" {
		patchDir := initOptions.kubeconfigDir + constant.PatchDir
		os.MkdirAll(path.Dir(patchDir), 0755)
		initOptions.patchesDir = patchDir
	}
	if err := util.WriteFile(initOptions.patchesDir+constant.KubeAPIServerPatch, string(kubeAPIServerPatch)); err != nil {
		klog.Errorf("Write file: %s, error: %v", constant.KubeAPIServerPatch, err)
		return err
	}

	edgeadmConfig.TunnelCloudToken = util.GetRandToken(32)

	return nil
}

// AddInitConfigFlags adds init flags bound to the config to the specified flagset
func AddInitConfigFlags(flagSet *flag.FlagSet, cfg *kubeadmapiv1beta3.InitConfiguration) {
	flagSet.StringVar(
		&cfg.LocalAPIEndpoint.AdvertiseAddress, options.APIServerAdvertiseAddress, cfg.LocalAPIEndpoint.AdvertiseAddress,
		"The IP address the API Server will advertise it's listening on. If not set the default network interface will be used.",
	)
	flagSet.Int32Var(
		&cfg.LocalAPIEndpoint.BindPort, options.APIServerBindPort, cfg.LocalAPIEndpoint.BindPort,
		"Port for the API Server to bind to.",
	)
	flagSet.StringVar(
		&cfg.NodeRegistration.Name, options.NodeName, cfg.NodeRegistration.Name,
		`Specify the node name.`,
	)
	flagSet.StringVar(
		&cfg.CertificateKey, options.CertificateKey, "",
		"Key used to encrypt the control-plane certificates in the kubeadm-certs Secret.",
	)
	cmdutil.AddCRISocketFlag(flagSet, &cfg.NodeRegistration.CRISocket)
}

// AddClusterConfigFlags adds cluster flags bound to the config to the specified flagset
func AddClusterConfigFlags(flagSet *flag.FlagSet, cfg *kubeadmapiv1beta3.ClusterConfiguration, featureGatesString *string) {
	flagSet.StringVar(
		&cfg.Networking.ServiceSubnet, options.NetworkingServiceSubnet, cfg.Networking.ServiceSubnet,
		"Use alternative range of IP address for service VIPs.",
	)
	flagSet.StringVar(
		&cfg.Networking.PodSubnet, options.NetworkingPodSubnet, cfg.Networking.PodSubnet,
		"Specify range of IP addresses for the pod network. If set, the control plane will automatically allocate CIDRs for every node.",
	)
	flagSet.StringVar(
		&cfg.Networking.DNSDomain, options.NetworkingDNSDomain, cfg.Networking.DNSDomain,
		`Use alternative domain for services, e.g. "myorg.internal".`,
	)

	flagSet.StringVar(
		&cfg.ControlPlaneEndpoint, options.ControlPlaneEndpoint, cfg.ControlPlaneEndpoint,
		`Specify a stable IP address or DNS name for the control plane.`,
	)

	options.AddKubernetesVersionFlag(flagSet, &cfg.KubernetesVersion)

	flagSet.StringVar(
		&cfg.CertificatesDir, options.CertificatesDir, cfg.CertificatesDir,
		`The path where to save and store the certificates.`,
	)
	flagSet.StringSliceVar(
		&cfg.APIServer.CertSANs, options.APIServerCertSANs, cfg.APIServer.CertSANs,
		`Optional extra Subject Alternative Names (SANs) to use for the API Server serving certificate. Can be both IP addresses and DNS names.`,
	)
	options.AddFeatureGatesStringFlag(flagSet, featureGatesString)
}

// AddInitOtherFlags adds init flags that are not bound to a configuration file to the given flagset
// Note: All flags that are not bound to the cfg object should be allowed in cmd/kubeadm/app/apis/kubeadm/validation/validation.go
func AddInitOtherFlags(flagSet *flag.FlagSet, initOptions *initOptions) {
	options.AddConfigFlag(flagSet, &initOptions.cfgPath)
	flagSet.StringSliceVar(
		&initOptions.ignorePreflightErrors, options.IgnorePreflightErrors, initOptions.ignorePreflightErrors,
		"A list of checks whose errors will be shown as warnings. Example: 'IsPrivilegedUser,Swap'. Value 'all' ignores errors from all checks.",
	)
	flagSet.BoolVar(
		&initOptions.skipTokenPrint, options.SkipTokenPrint, initOptions.skipTokenPrint,
		"Skip printing of the default bootstrap token generated by 'edgeadm init'.",
	)
	flagSet.BoolVar(
		&initOptions.dryRun, options.DryRun, initOptions.dryRun,
		"Don't apply any changes; just output what would be done.",
	)
	flagSet.BoolVar(
		&initOptions.uploadCerts, options.UploadCerts, initOptions.uploadCerts,
		"Upload control-plane certificates to the kubeadm-certs Secret.",
	)
	flagSet.BoolVar(
		&initOptions.skipCertificateKeyPrint, options.SkipCertificateKeyPrint, initOptions.skipCertificateKeyPrint,
		"Don't print the key used to encrypt the control-plane certificates.",
	)
	options.AddPatchesFlag(flagSet, &initOptions.patchesDir)
}

// newInitOptions returns a struct ready for being used for creating cmd init flags.
func newInitOptions() *initOptions {
	// initialize the public kubeadm config API by applying defaults
	externalInitCfg := &kubeadmapiv1beta3.InitConfiguration{}
	kubeadmscheme.Scheme.Default(externalInitCfg)

	externalClusterCfg := &kubeadmapiv1beta3.ClusterConfiguration{}
	kubeadmscheme.Scheme.Default(externalClusterCfg)

	// Create the options object for the bootstrap token-related flags, and override the default value for .Description
	bto := options.NewBootstrapTokenOptions()
	bto.Description = "The default bootstrap token generated by 'edgeadm init'."

	return &initOptions{
		externalInitCfg:    externalInitCfg,
		externalClusterCfg: externalClusterCfg,
		bto:                bto,
		kubeconfigDir:      kubeadmconstants.KubernetesDir,
		kubeconfigPath:     kubeadmconstants.GetAdminKubeConfigPath(),
		uploadCerts:        false,
	}
}

// newInitData returns a new initData struct to be used for the execution of the kubeadm init workflow.
// This func takes care of validating initOptions passed to the command, and then it converts
// options into the internal InitConfiguration type that is used as input all the phases in the kubeadm init workflow
func newInitData(cmd *cobra.Command, args []string, options *initOptions, out io.Writer) (*initData, error) {
	// Re-apply defaults to the public kubeadm API (this will set only values not exposed/not set as a flags)
	kubeadmscheme.Scheme.Default(options.externalInitCfg)
	kubeadmscheme.Scheme.Default(options.externalClusterCfg)

	// Validate standalone flags values and/or combination of flags and then assigns
	// validated values to the public kubeadm config API when applicable
	var err error
	if options.externalClusterCfg.FeatureGates, err = features.NewFeatureGate(&features.InitFeatureGates, options.featureGatesString); err != nil {
		return nil, err
	}

	if err = validation.ValidateMixedArguments(cmd.Flags()); err != nil {
		return nil, err
	}

	if err = options.bto.ApplyTo(options.externalInitCfg); err != nil {
		return nil, err
	}

	// Either use the config file if specified, or convert public kubeadm API to the internal InitConfiguration
	// and validates InitConfiguration
	cfg, err := configutil.LoadOrDefaultInitConfiguration(options.cfgPath, options.externalInitCfg, options.externalClusterCfg)
	if err != nil {
		return nil, err
	}

	ignorePreflightErrorsSet, err := validation.ValidateIgnorePreflightErrors(options.ignorePreflightErrors, cfg.NodeRegistration.IgnorePreflightErrors)
	if err != nil {
		return nil, err
	}
	// Also set the union of pre-flight errors to InitConfiguration, to provide a consistent view of the runtime configuration:
	cfg.NodeRegistration.IgnorePreflightErrors = ignorePreflightErrorsSet.List()

	// override node name and CRI socket from the command line options
	if options.externalInitCfg.NodeRegistration.Name != "" {
		cfg.NodeRegistration.Name = options.externalInitCfg.NodeRegistration.Name
	}
	if options.externalInitCfg.NodeRegistration.CRISocket != "" {
		cfg.NodeRegistration.CRISocket = options.externalInitCfg.NodeRegistration.CRISocket
	}

	// check node name parameter, the IPv4 format is not supported.
	if ret := net.ParseIP(cfg.NodeRegistration.Name); ret != nil {
		return nil, errors.New("invalid node name, the IPv4 format is not supported")
	}

	if err := configutil.VerifyAPIServerBindAddress(cfg.LocalAPIEndpoint.AdvertiseAddress); err != nil {
		return nil, err
	}
	if err := features.ValidateVersion(features.InitFeatureGates, cfg.FeatureGates, cfg.KubernetesVersion); err != nil {
		return nil, err
	}

	// if dry running creates a temporary folder for saving kubeadm generated files
	dryRunDir := ""
	if options.dryRun {
		// the KUBEADM_INIT_DRYRUN_DIR environment variable allows overriding the dry-run temporary
		// directory from the command line. This makes it possible to run "kubeadm init" integration
		// tests without root.
		if dryRunDir, err = kubeadmconstants.CreateTempDirForKubeadm(os.Getenv("KUBEADM_INIT_DRYRUN_DIR"), "kubeadm-init-dryrun"); err != nil {
			return nil, errors.Wrap(err, "couldn't create a temporary directory")
		}
	}

	// Checks if an external CA is provided by the user (when the CA Cert is present but the CA Key is not)
	externalCA, err := certsphase.UsingExternalCA(&cfg.ClusterConfiguration)
	if externalCA {
		// In case the certificates signed by CA (that should be provided by the user) are missing or invalid,
		// returns, because kubeadm can't regenerate them without the CA Key
		if err != nil {
			return nil, errors.Wrapf(err, "invalid or incomplete external CA")
		}

		// Validate that also the required kubeconfig files exists and are invalid, because
		// kubeadm can't regenerate them without the CA Key
		kubeconfigDir := options.kubeconfigDir
		if options.dryRun {
			kubeconfigDir = dryRunDir
		}
		if err := kubeconfigphase.ValidateKubeconfigsForExternalCA(kubeconfigDir, cfg); err != nil {
			return nil, err
		}
	}

	// Checks if an external Front-Proxy CA is provided by the user (when the Front-Proxy CA Cert is present but the Front-Proxy CA Key is not)
	externalFrontProxyCA, err := certsphase.UsingExternalFrontProxyCA(&cfg.ClusterConfiguration)
	if externalFrontProxyCA {
		// In case the certificates signed by Front-Proxy CA (that should be provided by the user) are missing or invalid,
		// returns, because kubeadm can't regenerate them without the Front-Proxy CA Key
		if err != nil {
			return nil, errors.Wrapf(err, "invalid or incomplete external front-proxy CA")
		}
	}

	if options.uploadCerts && (externalCA || externalFrontProxyCA) {
		return nil, errors.New("can't use upload-certs with an external CA or an external front-proxy CA")
	}

	return &initData{
		cfg:                     cfg,
		certificatesDir:         cfg.CertificatesDir,
		skipTokenPrint:          options.skipTokenPrint,
		dryRun:                  options.dryRun,
		dryRunDir:               dryRunDir,
		kubeconfigDir:           options.kubeconfigDir,
		kubeconfigPath:          options.kubeconfigPath,
		ignorePreflightErrors:   ignorePreflightErrorsSet,
		externalCA:              externalCA,
		outputWriter:            out,
		uploadCerts:             options.uploadCerts,
		skipCertificateKeyPrint: options.skipCertificateKeyPrint,
		kustomizeDir:            options.kustomizeDir,
		patchesDir:              options.patchesDir,
	}, nil
}

// UploadCerts returns Uploadcerts flag.
func (d *initData) UploadCerts() bool {
	return d.uploadCerts
}

// CertificateKey returns the key used to encrypt the certs.
func (d *initData) CertificateKey() string {
	return d.cfg.CertificateKey
}

// SetCertificateKey set the key used to encrypt the certs.
func (d *initData) SetCertificateKey(key string) {
	d.cfg.CertificateKey = key
}

// SkipCertificateKeyPrint returns the skipCertificateKeyPrint flag.
func (d *initData) SkipCertificateKeyPrint() bool {
	return d.skipCertificateKeyPrint
}

// Cfg returns initConfiguration.
func (d *initData) Cfg() *kubeadmapi.InitConfiguration {
	return d.cfg
}

// DryRun returns the DryRun flag.
func (d *initData) DryRun() bool {
	return d.dryRun
}

// SkipTokenPrint returns the SkipTokenPrint flag.
func (d *initData) SkipTokenPrint() bool {
	return d.skipTokenPrint
}

// IgnorePreflightErrors returns the IgnorePreflightErrors flag.
func (d *initData) IgnorePreflightErrors() sets.String {
	return d.ignorePreflightErrors
}

// CertificateWriteDir returns the path to the certificate folder or the temporary folder path in case of DryRun.
func (d *initData) CertificateWriteDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return d.certificatesDir
}

// CertificateDir returns the CertificateDir as originally specified by the user.
func (d *initData) CertificateDir() string {
	return d.certificatesDir
}

// KubeConfigDir returns the path of the Kubernetes configuration folder or the temporary folder path in case of DryRun.
func (d *initData) KubeConfigDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return d.kubeconfigDir
}

// KubeConfigPath returns the path to the kubeconfig file to use for connecting to Kubernetes
func (d *initData) KubeConfigPath() string {
	if d.dryRun {
		d.kubeconfigPath = filepath.Join(d.dryRunDir, kubeadmconstants.AdminKubeConfigFileName)
	}
	return d.kubeconfigPath
}

// ManifestDir returns the path where manifest should be stored or the temporary folder path in case of DryRun.
func (d *initData) ManifestDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return kubeadmconstants.GetStaticPodDirectory()
}

// KubeletDir returns path of the kubelet configuration folder or the temporary folder in case of DryRun.
func (d *initData) KubeletDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return kubeadmconstants.KubeletRunDirectory
}

// ExternalCA returns true if an external CA is provided by the user.
func (d *initData) ExternalCA() bool {
	return d.externalCA
}

// OutputWriter returns the io.Writer used to write output to by this command.
func (d *initData) OutputWriter() io.Writer {
	return d.outputWriter
}

// Client returns a Kubernetes client to be used by kubeadm.
// This function is implemented as a singleton, thus avoiding to recreate the client when it is used by different phases.
// Important. This function must be called after the admin.conf kubeconfig file is created.
func (d *initData) Client() (clientset.Interface, error) {
	if d.client == nil {
		if d.dryRun {
			svcSubnetCIDR, err := kubeadmconstants.GetKubernetesServiceCIDR(d.cfg.Networking.ServiceSubnet, features.Enabled(d.cfg.FeatureGates, features.IPv6DualStack))
			if err != nil {
				return nil, errors.Wrapf(err, "unable to get internal Kubernetes Service IP from the given service CIDR (%s)", d.cfg.Networking.ServiceSubnet)
			}
			// If we're dry-running, we should create a faked client that answers some GETs in order to be able to do the full init flow and just logs the rest of requests
			dryRunGetter := apiclient.NewInitDryRunGetter(d.cfg.NodeRegistration.Name, svcSubnetCIDR.String())
			d.client = apiclient.NewDryRunClient(dryRunGetter, os.Stdout)
		} else {
			// If we're acting for real, we should create a connection to the API server and wait for it to come up
			var err error
			d.client, err = kubeconfigutil.ClientSetFromFile(d.KubeConfigPath())
			if err != nil {
				return nil, err
			}
		}
	}
	return d.client, nil
}

// Tokens returns an array of token strings.
func (d *initData) Tokens() []string {
	tokens := []string{}
	for _, bt := range d.cfg.BootstrapTokens {
		tokens = append(tokens, bt.Token.String())
	}
	return tokens
}

// KustomizeDir returns the folder where kustomize patches for static pod manifest are stored
func (d *initData) KustomizeDir() string {
	return d.kustomizeDir
}

// PatchesDir returns the folder where patches for components are stored
func (d *initData) PatchesDir() string {
	return d.patchesDir
}

func printJoinCommand(out io.Writer, adminKubeConfigPath, token string, i *initData) error {
	joinControlPlaneCommand, err := cmdutil.GetJoinControlPlaneCommand(adminKubeConfigPath, token, i.CertificateKey(), i.skipTokenPrint, i.skipCertificateKeyPrint)
	if err != nil {
		return err
	}

	joinWorkerCommand, err := cmdutil.GetJoinWorkerCommand(adminKubeConfigPath, token, i.skipTokenPrint)
	if err != nil {
		return err
	}

	if !steps.EdgeadmConf.IsEnableEdge {
		joinWorkerCommand = fmt.Sprintf("%s --%s=false", joinWorkerCommand, constant.ISEnableEdge)
		joinControlPlaneCommand = fmt.Sprintf("%s --%s=false", joinControlPlaneCommand, constant.ISEnableEdge)
	}

	if steps.EdgeadmConf.IsEnableEdge {
		joinAddr := strings.TrimSpace(util.GetStringInBetween(joinWorkerCommand, "kubeadm join", "--token"))
		joinAddr = joinAddr[:strings.Index(joinAddr, ":")]
		if len(i.Cfg().APIServer.CertSANs) > 0 {
			joinWorkerCommand = strings.Replace(joinWorkerCommand, joinAddr, i.Cfg().APIServer.CertSANs[0], -1)
		}
	}

	joinWorkerCommand = fmt.Sprintf("%s \\\n%s--%s <%s>\n", joinWorkerCommand, constant.ControlFormat, constant.InstallPkgPath, constant.InstallPkgPathNote)
	joinControlPlaneCommand = fmt.Sprintf("%s \\\n%s--%s <%s>\n", joinControlPlaneCommand, constant.ControlFormat, constant.InstallPkgPath, constant.InstallPkgPathNote)

	ctx := map[string]interface{}{
		"KubeConfigPath":          adminKubeConfigPath,
		"ControlPlaneEndpoint":    i.Cfg().ControlPlaneEndpoint,
		"UploadCerts":             i.uploadCerts,
		"joinControlPlaneCommand": strings.ReplaceAll(joinControlPlaneCommand, "kubeadm", "edgeadm"),
		"joinWorkerCommand":       strings.ReplaceAll(joinWorkerCommand, "kubeadm", "edgeadm"),
	}
	return initDoneTempl.Execute(out, ctx)
}

// showJoinCommand prints the join command after all the phases in init have finished
func showJoinCommand(i *initData, out io.Writer) error {
	adminKubeConfigPath := i.KubeConfigPath()

	// Prints the join command, multiple times in case the user has multiple tokens
	for _, token := range i.Tokens() {
		if err := printJoinCommand(out, adminKubeConfigPath, token, i); err != nil {
			return errors.Wrap(err, "failed to print join command")
		}
	}

	return nil
}
