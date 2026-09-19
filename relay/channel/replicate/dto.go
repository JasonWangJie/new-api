package replicate

import "github.com/QuantumNous/new-api/common"

type PredictionResponse struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Output any              `json:"output"`
	Error  *PredictionError `json:"error"`
}

type PredictionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

func (predictionError *PredictionError) UnmarshalJSON(data []byte) error {
	var message string
	if err := common.Unmarshal(data, &message); err == nil {
		predictionError.Message = message
		return nil
	}
	type errorFields PredictionError
	return common.Unmarshal(data, (*errorFields)(predictionError))
}

type FileUploadResponse struct {
	Urls struct {
		Get string `json:"get"`
	} `json:"urls"`
}
