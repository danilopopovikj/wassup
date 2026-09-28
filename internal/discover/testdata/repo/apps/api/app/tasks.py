from celery import Celery

app = Celery("app", broker="redis://redis.bookstore:6379/1")


@app.task(queue="exports")
def export_report(report_id):
    return post("https://api.stripe.com/v1/invoices", report_id)
