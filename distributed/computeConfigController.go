package distributed

func GetCustomerList() []string {
	return customers
}

type CustomerComputeConfig struct {
	Pghost       string
	ComputeShare float32
}

var customerComputeConfigs map[string]CustomerComputeConfig
var customers []string

func GetComputeConfig(customer string) CustomerComputeConfig {
	return customerComputeConfigs[customer]

}

func GetAllCustomersComputeConfig() map[string]CustomerComputeConfig {
	return customerComputeConfigs
}

func Setup() {
	customerComputeConfigs = make(map[string]CustomerComputeConfig)

	customers = []string{"workspaceID"}
	customerComputeConfigs["workspaceID"] = CustomerComputeConfig{
		Pghost:       "127.0.0.1",
		ComputeShare: 1,
	}
}