import json

from upstream import SSEEvent, parse_sse_event


def test_sse_event_parser_counts_delta_not_json_metadata():
    event = b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n'
    parsed = parse_sse_event(event)
    assert isinstance(parsed, SSEEvent)
    assert parsed.output_characters == 5
    assert parsed.usage is None


def test_sse_event_parser_reads_final_usage():
    event = b'data: {"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}\n\n'
    assert parse_sse_event(event).usage["total_tokens"] == 7


def test_sse_done_and_invalid_events_are_safe():
    assert parse_sse_event(b"data: [DONE]\n\n").done is True
    assert parse_sse_event(b"event: ping\n\n") == SSEEvent()
