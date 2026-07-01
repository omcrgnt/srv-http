// Package use registers shared HTTP metrics (slok recorder) in unique.Global.
//
// Import for side effects at the app composition root:
//
//	import _ "github.com/omcrgnt/srv-http/use"
package use

import (
	srvhttp "github.com/omcrgnt/srv-http"
	"github.com/omcrgnt/res/unique"
)

func init() {
	unique.MustAddFixed(&srvhttp.HTTPMetrics{})
}
