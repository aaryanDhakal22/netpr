from selenium import webdriver
from django.test import LiveServerTestCase
from selenium.webdriver.common.by import By 
from time import sleep


class PrintJobs(LiveServerTestCase):
    
    def setUp(self):
        self.driver = webdriver.Firefox()
        
    
    def tearDown(self):
        self.driver.quit()

    def test_can_send_print_jobs(self):

        # User goes to the website
        self.driver.get("http://localhost:8000")
        sleep(10)
        # User types a title
        title_box = self.driver.find_element(By.ID, "inp-title")
        title_box.send_keys("Test Print")

        # User types a password "P@ssw0rd"
        password_box = self.driver.find_element(By.ID, "inp-password")
        password_box.send_keys("P@ssw0rd")

        # User selects a file

        # User clicks print
        print_button = self.driver.find_element(By.ID,"inp-submit")

        # User sees that it was approved with a "Print was Successfull" page 
        ret_msg = self.driver.find_element(By.ID,"response-text")
        self.assertEqual(ret_msg,"Print Succesfull")
        # User is redirected to the homepage