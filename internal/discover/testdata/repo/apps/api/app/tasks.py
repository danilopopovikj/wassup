import boto3
from celery import Celery

s3 = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"])

app = Celery("app", broker="redis://redis.bookstore:6379/1")


@app.task(queue="exports")
def export_report(report_id):
    return post("https://api.stripe.com/v1/invoices", report_id)


@app.task(queue="exports")
def upload_export(report_id, body):
    s3.put_object(Bucket="bookstore-media", Key=f"exports/{report_id}.csv", Body=body)
