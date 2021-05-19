package hosts

type Metadata struct {
	Cluster     string `mapstructure:"ha-cluster,omitempty"`
	Environment string `mapstructure:"sap-environment,omitempty"`
	Landscape   string `mapstructure:"sap-landscape,omitempty"`
	SAPSystem   string `mapstructure:"sap-system,omitempty"`
}