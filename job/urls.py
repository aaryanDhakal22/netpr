
from django.urls import path
from .views import homepage, upload_file
urlpatterns = [
    path('',homepage),
    path('print/',upload_file)
]
