
from django.urls import path
from .views import homepage,print_job
urlpatterns = [
    path('',homepage),
    path('print/',print_job)
]
