package application

import (
	"flag"
	"fmt"

	"github.com/spf13/viper"
	"go.uber.org/multierr"

	"github.com/Kindling-project/kindling/collector/pkg/component"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer/cpuanalyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer/loganalyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer/network"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer/tcpconnectanalyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/analyzer/tcpmetricanalyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer/exporter/cameraexporter"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer/exporter/logexporter"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer/exporter/otelexporter"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer/processor/aggregateprocessor"
	"github.com/Kindling-project/kindling/collector/pkg/component/consumer/processor/k8sprocessor"
	"github.com/Kindling-project/kindling/collector/pkg/component/controller"
	"github.com/Kindling-project/kindling/collector/pkg/component/receiver"
	"github.com/Kindling-project/kindling/collector/pkg/component/receiver/cgoreceiver"
)

type Application struct {
	viper             *viper.Viper
	controllerFactory *controller.ControllerFactory
	componentsFactory *ComponentsFactory
	telemetry         *component.TelemetryManager
	receiver          receiver.Receiver
	analyzerManager   *analyzer.Manager
}

func New() (*Application, error) {
	app := &Application{
		viper:             viper.New(),
		componentsFactory: NewComponentsFactory(),
		telemetry:         component.NewTelemetryManager(),
		controllerFactory: &controller.ControllerFactory{},
	}
	app.registerFactory()
	// Initialize flags
	configPath := flag.String("config", "kindling-collector-config.yml", "Configuration file")
	flag.Parse()
	err := app.readInConfig(*configPath)
	if err != nil {
		return nil, fmt.Errorf("fail to read configuration: %w", err)
	}
	// Build processing pipeline
	err = app.buildPipeline()
	if err != nil {
		return nil, fmt.Errorf("failed to build pipeline: %w", err)
	}
	return app, nil
}

func (a *Application) Run() error {
	err := a.analyzerManager.StartAll(a.telemetry.GetGlobalTelemetryTools().Logger)
	if err != nil {
		return fmt.Errorf("failed to start application: %v", err)
	}
	// Wait until the receiver shutdowns
	err = a.receiver.Start()
	if err != nil {
		return fmt.Errorf("failed to start application: %v", err)
	}
	return nil
}

func (a *Application) Shutdown() error {
	return multierr.Combine(a.receiver.Shutdown(), a.analyzerManager.ShutdownAll(a.telemetry.GetGlobalTelemetryTools().Logger))
}

func (a *Application) registerFactory() {
	a.componentsFactory.RegisterReceiver(cgoreceiver.Cgo, cgoreceiver.NewCgoReceiver, &cgoreceiver.Config{})
	a.componentsFactory.RegisterAnalyzer(network.Network.String(), network.NewNetworkAnalyzer, &network.Config{})
	a.componentsFactory.RegisterAnalyzer(cpuanalyzer.CpuProfile.String(), cpuanalyzer.NewCpuAnalyzer, cpuanalyzer.NewDefaultConfig())
	a.componentsFactory.RegisterProcessor(k8sprocessor.K8sMetadata, k8sprocessor.NewKubernetesProcessor, &k8sprocessor.DefaultConfig)
	a.componentsFactory.RegisterExporter(otelexporter.Otel, otelexporter.NewExporter, &otelexporter.Config{})
	a.componentsFactory.RegisterAnalyzer(tcpmetricanalyzer.TcpMetric.String(), tcpmetricanalyzer.NewTcpMetricAnalyzer, &tcpmetricanalyzer.Config{})
	a.componentsFactory.RegisterExporter(logexporter.Type, logexporter.New, &logexporter.Config{})
	a.componentsFactory.RegisterAnalyzer(loganalyzer.Type.String(), loganalyzer.New, &loganalyzer.Config{})
	a.componentsFactory.RegisterProcessor(aggregateprocessor.Type, aggregateprocessor.New, aggregateprocessor.NewDefaultConfig())
	a.componentsFactory.RegisterAnalyzer(tcpconnectanalyzer.Type.String(), tcpconnectanalyzer.New, tcpconnectanalyzer.NewDefaultConfig())
	a.componentsFactory.RegisterExporter(cameraexporter.Type, cameraexporter.New, cameraexporter.NewDefaultConfig())
}

// 读取配置
func (a *Application) readInConfig(path string) error {
	a.viper.SetConfigFile(path)
	err := a.viper.ReadInConfig()
	if err != nil { // Handle errors reading the config file
		return fmt.Errorf("error happened while reading config file: %w", err)
	}
	a.telemetry.ConstructConfig(a.viper)
	err = a.componentsFactory.ConstructConfig(a.viper)
	_ = a.controllerFactory.ConstructConfig(a.viper, a.telemetry.GetGlobalTelemetryTools())
	if err != nil {
		return fmt.Errorf("error happened while constructing config: %w", err)
	}
	return nil
}

// buildPipeline builds a event processing pipeline based on hard-code.
func (a *Application) buildPipeline() error {
	// 初始化exporter、processor、analyzer等等
	// TODO: Build pipeline via configuration to implement dependency injection
	// Initialize exporters
	otelExporterFactory := a.componentsFactory.Exporters[otelexporter.Otel]
	otelExporter := otelExporterFactory.NewFunc(otelExporterFactory.Config, a.telemetry.GetTelemetryTools(otelexporter.Otel))
	cameraExporterFactory := a.componentsFactory.Exporters[cameraexporter.Type]
	cameraExporter := cameraExporterFactory.NewFunc(cameraExporterFactory.Config, a.telemetry.GetTelemetryTools(cameraexporter.Type))
	// Initialize all processors
	// 1. DataGroup Aggregator
	aggregateProcessorFactory := a.componentsFactory.Processors[aggregateprocessor.Type]
	aggregateProcessor := aggregateProcessorFactory.NewFunc(aggregateProcessorFactory.Config, a.telemetry.GetTelemetryTools(aggregateprocessor.Type), otelExporter)
	// 2. Kubernetes metadata processor
	k8sProcessorFactory := a.componentsFactory.Processors[k8sprocessor.K8sMetadata]
	k8sMetadataProcessor := k8sProcessorFactory.NewFunc(k8sProcessorFactory.Config, a.telemetry.GetTelemetryTools(k8sprocessor.K8sMetadata), aggregateProcessor)
	// Initialize all analyzers
	// 1. Common network request analyzer  网络请求分析
	networkAnalyzerFactory := a.componentsFactory.Analyzers[network.Network.String()]
	// Now NetworkAnalyzer must be initialized before any other analyzers, because it will
	// use its configuration to initialize the conntracker module which is also used by others.
	networkAnalyzer := networkAnalyzerFactory.NewFunc(networkAnalyzerFactory.Config, a.telemetry.GetTelemetryTools(network.Network.String()), []consumer.Consumer{k8sMetadataProcessor})
	// 2. Layer 4 TCP events analyzer 4层tcp解析
	aggregateProcessorForTcp := aggregateProcessorFactory.NewFunc(aggregateProcessorFactory.Config, a.telemetry.GetTelemetryTools(aggregateprocessor.Type), otelExporter)
	k8sMetadataProcessor2 := k8sProcessorFactory.NewFunc(k8sProcessorFactory.Config, a.telemetry.GetTelemetryTools(aggregateprocessor.Type), aggregateProcessorForTcp)
	tcpAnalyzerFactory := a.componentsFactory.Analyzers[tcpmetricanalyzer.TcpMetric.String()]
	tcpAnalyzer := tcpAnalyzerFactory.NewFunc(tcpAnalyzerFactory.Config, a.telemetry.GetTelemetryTools(tcpconnectanalyzer.Type.String()), []consumer.Consumer{k8sMetadataProcessor2})
	tcpConnectAnalyzerFactory := a.componentsFactory.Analyzers[tcpconnectanalyzer.Type.String()]
	tcpConnectAnalyzer := tcpConnectAnalyzerFactory.NewFunc(tcpConnectAnalyzerFactory.Config, a.telemetry.GetTelemetryTools(tcpconnectanalyzer.Type.String()), []consumer.Consumer{k8sMetadataProcessor})

	cpuAnalyzerFactory := a.componentsFactory.Analyzers[cpuanalyzer.CpuProfile.String()]
	cpuAnalyzer := cpuAnalyzerFactory.NewFunc(cpuAnalyzerFactory.Config, a.telemetry.GetTelemetryTools(cpuanalyzer.CpuProfile.String()), []consumer.Consumer{cameraExporter})

	// Initialize receiver packaged with multiple analyzers
	analyzerManager, err := analyzer.NewManager(networkAnalyzer, tcpAnalyzer, tcpConnectAnalyzer, cpuAnalyzer)
	if err != nil {
		return fmt.Errorf("error happened while creating analyzer manager: %w", err)
	}
	a.analyzerManager = analyzerManager

	cgoReceiverFactory := a.componentsFactory.Receivers[cgoreceiver.Cgo]
	cgoReceiver := cgoReceiverFactory.NewFunc(cgoReceiverFactory.Config, a.telemetry.GetTelemetryTools(cgoreceiver.Cgo), analyzerManager)
	a.receiver = cgoReceiver

	a.controllerFactory.RegistModule("profile",
		cpuAnalyzer.(*cpuanalyzer.CpuAnalyzer).ProfileModule,
		cgoReceiver.(*cgoreceiver.CgoReceiver).ProfileModule,
	)

	return nil
}
