package main

// openaiToGeminiRequest converts an OpenAI chat request to a Gemini generateContent request.
func openaiToGeminiRequest(req *OpenAIChatRequest) *GeminiGenerateRequest {
	gemReq := &GeminiGenerateRequest{}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			// System messages go to systemInstruction
			gemReq.SystemInstruction = &GeminiContent{
				Parts: []GeminiPart{{Text: msg.Content}},
			}
		case "user":
			gemReq.Contents = append(gemReq.Contents, GeminiContent{
				Role:  "user",
				Parts: []GeminiPart{{Text: msg.Content}},
			})
		case "assistant":
			gemReq.Contents = append(gemReq.Contents, GeminiContent{
				Role:  "model",
				Parts: []GeminiPart{{Text: msg.Content}},
			})
		}
	}

	// Map generation parameters
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil || len(req.Stop) > 0 {
		gc := &GeminiGenerationConfig{}
		if req.Temperature != nil {
			gc.Temperature = req.Temperature
		}
		if req.TopP != nil {
			gc.TopP = req.TopP
		}
		if req.MaxTokens != nil {
			gc.MaxOutputTokens = req.MaxTokens
		}
		if len(req.Stop) > 0 {
			gc.StopSequences = req.Stop
		}
		gemReq.GenerationConfig = gc
	}

	return gemReq
}
