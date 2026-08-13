package notify

import (
	"encoding/json"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

// decodeDiagnosis re-validates the stored structured diagnosis before it is
// published. A finding that no longer satisfies the domain contract is skipped
// rather than posted, because a pull request comment is a durable public claim.
func decodeDiagnosis(encoded string) (domain.Diagnosis, bool) {
	var diagnosis domain.Diagnosis
	if json.Unmarshal([]byte(encoded), &diagnosis) != nil {
		return domain.Diagnosis{}, false
	}
	if diagnosis.Validate() != nil {
		return domain.Diagnosis{}, false
	}
	return diagnosis, true
}
