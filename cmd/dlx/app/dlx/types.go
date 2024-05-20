package dlx

import "github.com/v3io/scaler/pkg/scalertypes"

type DLXOptions struct {
	scalertypes.DLXOptions

	TCPListenAddress string
}

const NuclioResourceLabelKeyScaleToZeroPort = "nuclio.io/scale-to-zero-port"
