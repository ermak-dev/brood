"""Вывод проверенных полей MR без превращения данных API в доказательство git."""


def report(data):
    print(f"MR !{data['iid']}: {data['state']}")
    print(f"source HEAD: {data['sha']}")
    print(f"merge commit SHA: {data.get('merge_commit_sha') or '(not provided)'}")
    if data["state"] == "merged":
        print("API reports merged; Git/main not verified.")
    else:
        print("Merge completion not confirmed by API; Git/main not verified.")
