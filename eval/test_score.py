import unittest
from score import score


class ScoreTests(unittest.TestCase):
    def test_missing_labels_are_unknown(self):
        result = score([{"id": "a"}], [{"id": "a", "status": "SUCCEEDED"}])
        self.assertIsNone(result["human_grounded_rate"])
        self.assertIsNone(result["citation_interval_recall_iou_0_3"])
        self.assertEqual(result["runs_with_unknown_usage"], 1)

    def test_interval_and_human_scores_are_separate(self):
        result = score([{ "id": "a", "expected_intervals": [[0, 1000]] }], [{
            "id": "a", "status": "SUCCEEDED", "latency_ms": 200,
            "human_grounded": False, "usage_known": True,
            "result": {"mock": True, "evidence": [{"start_ms": 0, "end_ms": 900}]},
        }])
        self.assertEqual(result["citation_interval_recall_iou_0_3"], 1)
        self.assertEqual(result["human_grounded_rate"], 0)
        self.assertEqual(result["mock_runs"], 1)


if __name__ == "__main__":
    unittest.main()
