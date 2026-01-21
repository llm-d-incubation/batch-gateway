type LLMClient interface {
	Generate(ctx context.Context, req *LLMRequest) (*LLMResponse, error)
}

type LLMRequest struct {
	RequestID string                 // unique request id set by user
	Model  string // model id (also inside Params)
	Params map[string]interface{} // parameters
}

// Request Params example openai chat completion with tool calls:
// {
//   "model": "gpt-4.1",
//   "messages": [
//     {
//       "role": "user",
//       "content": "What is the weather like in Boston today?"
//     }
//   ],
//   "tools": [
//     {
//       "type": "function",
//       "function": {
//         "name": "get_current_weather",
//         "description": "Get the current weather in a given location",
//         "parameters": {
//           "type": "object",
//           "properties": {
//             "location": {
//               "type": "string",
//               "description": "The city and state, e.g. San Francisco, CA"
//             },
//             "unit": {
//               "type": "string",
//               "enum": ["celsius", "fahrenheit"]
//             }
//           },
//           "required": ["location"]
//         }
//       }
//     }
//   ],
//   "tool_choice": "auto"
// }
type LLMResponse struct {
	RequestID  string
	Response []byte
	RawData interface{}
}

// Response example for openai chat completion with tool calls:
// {
//   "id": "chatcmpl-abc123",
//   "object": "chat.completion",
//   "created": 1699896916,
//   "model": "gpt-4o-mini",
//   "choices": [
//     {
//       "index": 0,
//       "message": {
//         "role": "assistant",
//         "content": null,
//         "tool_calls": [
//           {
//             "id": "call_abc123",
//             "type": "function",
//             "function": {
//               "name": "get_current_weather",
//               "arguments": "{\n\"location\": \"Boston, MA\"\n}"
//             }
//           }
//         ]
//       },
//       "logprobs": null,
//       "finish_reason": "tool_calls"
//     }
//   ],
//   "usage": {
//     "prompt_tokens": 82,
//     "completion_tokens": 17,
//     "total_tokens": 99,
//     "completion_tokens_details": {
//       "reasoning_tokens": 0,
//       "accepted_prediction_tokens": 0,
//       "rejected_prediction_tokens": 0
//     }
//   }
// }
