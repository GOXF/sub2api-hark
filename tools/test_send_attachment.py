import base64
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError

from send_attachment import request_body, send


class AttachmentSenderTest(unittest.TestCase):
    def test_all_selected_types_are_transmitted_without_local_paths(self):
        names = ("test.xls", "test.xlsx", "test.pdf", "test.docx", "test.pptx",
                 "test.png", "test.jpeg", "test.txt", "test.csv", "test.md",
                 "test.mp3", "test.mp4", "test.zip", "file.future", "README")
        with tempfile.TemporaryDirectory() as folder:
            files = [Path(folder) / name for name in names]
            for index, path in enumerate(files):
                path.write_bytes(bytes((0, index, 255)))
            wire = request_body("fixture", "read the files", files)
            self.assertNotIn(str(folder).encode(), wire)
            parts = json.loads(wire)["input"][0]["content"][1:]
            self.assertEqual([part["filename"] for part in parts], list(names))
            for index, part in enumerate(parts):
                self.assertEqual(part["type"], "input_file")
                self.assertEqual(base64.b64decode(part["file_data"]), bytes((0, index, 255)))

    def test_reports_only_the_public_format_error(self):
        url = "https://example.invalid/v1/responses"
        with patch("send_attachment.build_opener") as opener:
            error = {"error": {"code": "unsupported_file_type", "message": "不支持 .fixture 格式的文件"}}
            opener.return_value.open.side_effect = HTTPError(
                url, 415, "Unsupported Media Type", None, io.BytesIO(json.dumps(error).encode()))
            with self.assertRaisesRegex(ValueError, r"^不支持 \.fixture 格式的文件$"):
                send(url, "fixture-key", b"{}")

    def test_rejects_insecure_remote_destination_before_network(self):
        with patch("send_attachment.build_opener") as opener:
            with self.assertRaises(ValueError):
                send("http://example.invalid/v1/responses", "fixture", b"{}")
            opener.assert_not_called()


if __name__ == "__main__":
    unittest.main()
