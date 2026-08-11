package logger

type LogField map[string]any

func expandLogField(m LogField) (res []any) {
	for k, v := range m {
		res = append(res, k, v)
	}
	return
}

func findLogField(keyAndValue []any) ([]any, bool) {
	if len(keyAndValue) == 1 && keyAndValue[0] != nil {
		v, ok := keyAndValue[0].(LogField)
		if ok {
			return expandLogField(v), true
		}
	}
	return nil, false
}
